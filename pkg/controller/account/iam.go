package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/go-logr/logr"
	awsv1alpha1 "github.com/openshift/aws-account-operator/pkg/apis/aws/v1alpha1"
	"github.com/openshift/aws-account-operator/pkg/controller/utils"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/openshift/aws-account-operator/pkg/awsclient"
	"github.com/openshift/aws-account-operator/pkg/credentialwatcher"
	"k8s.io/apimachinery/pkg/types"
)

// Type for JSON response from Federation end point
type awsSigninTokenResponse struct {
	SigninToken string
}

// Type that represents JSON object of an AWS permissions statement
type awsStatement struct {
	Effect    string                 `json:"Effect"`
	Action    []string               `json:"Action"`
	Resource  []string               `json:"Resource,omitempty"`
	Principal *awsv1alpha1.Principal `json:"Principal,omitempty"`
}

// PolicyDocument represents JSON object of an AWS Policy Document
type PolicyDocument struct {
	Version   string
	Statement []StatementEntry
}

// StatementEntry represents JSON of a statement in a policy doc
type StatementEntry struct {
	Effect   string
	Action   []string
	Resource string
}

// RequestSigninToken makes a HTTP request to retrieve an AWS Signin Token
// via the AWS Federation endpoint
func RequestSigninToken(reqLogger logr.Logger, awsclient awsclient.Client, DurationSeconds *int64, FederatedUserName *string, PolicyArns []*sts.PolicyDescriptorType, STSCredentials *sts.AssumeRoleOutput) (string, error) {
	// URL for building Federated Signin queries
	federationEndpointURL := "https://signin.aws.amazon.com/federation"

	// Get Federated token credentials to build console URL
	GetFederationTokenOutput, err := getFederationToken(reqLogger, awsclient, DurationSeconds, FederatedUserName, PolicyArns)

	if err != nil {
		return "", err
	}

	signinTokenResponse, err := getSigninToken(reqLogger, federationEndpointURL, GetFederationTokenOutput)

	if err != nil {
		return "", err
	}

	signedFederationURL, err := formatSigninURL(reqLogger, federationEndpointURL, signinTokenResponse.SigninToken)

	if err != nil {
		return "", err
	}

	// Return Signin Token
	return signedFederationURL.String(), nil

}

// getFederationToken gets the Federation Token from AWS.
func getFederationToken(reqLogger logr.Logger, awsclient awsclient.Client, DurationSeconds *int64, FederatedUserName *string, PolicyArns []*sts.PolicyDescriptorType) (*sts.GetFederationTokenOutput, error) {

	GetFederationTokenInput := sts.GetFederationTokenInput{
		DurationSeconds: DurationSeconds,
		Name:            FederatedUserName,
		PolicyArns:      PolicyArns,
	}

	// Get Federated token credentials to build console URL
	GetFederationTokenOutput, err := awsclient.GetFederationToken(&GetFederationTokenInput)

	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			// Get error details
			reqLogger.Error(err, fmt.Sprintf("Error: %s, %s", awsErr.Code(), awsErr.Message()))
			return GetFederationTokenOutput, err
		}

		return GetFederationTokenOutput, err
	}

	if GetFederationTokenOutput == nil {

		reqLogger.Error(awsv1alpha1.ErrFederationTokenOutputNil, fmt.Sprintf("Federation Token Output: %+v", GetFederationTokenOutput))
		return GetFederationTokenOutput, awsv1alpha1.ErrFederationTokenOutputNil

	}

	return GetFederationTokenOutput, nil

}

// formatSigninURL build and format the signin URL to be used in the secret
func formatSigninURL(reqLogger logr.Logger, federationEndpointURL, signinToken string) (*url.URL, error) {
	// URLs for building Federated Signin queries
	awsConsoleURL := "https://console.aws.amazon.com/"
	issuer := "Red Hat SRE"

	signinFederationURL, err := url.Parse(federationEndpointURL)

	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Malformed URL: %s", err.Error()))
		return signinFederationURL, err
	}

	signinParams := url.Values{}

	signinParams.Add("Action", "login")
	signinParams.Add("Destination", awsConsoleURL)
	signinParams.Add("Issuer", issuer)
	signinParams.Add("SigninToken", signinToken)

	signinFederationURL.RawQuery = signinParams.Encode()

	return signinFederationURL, nil

}

// CreateSecret creates a secret for placing IAM Credentials
// Takes a logger, the desired name of the secret, the Account CR
// that will own the secret, and pointer to an empty secret object to fill
func (r *ReconcileAccount) CreateSecret(reqLogger logr.Logger, account *awsv1alpha1.Account, secret *corev1.Secret) error {

	// Set controller as owner of secret
	if err := controllerutil.SetControllerReference(account, secret, r.scheme); err != nil {
		return err
	}

	createErr := r.Client.Create(context.TODO(), secret)
	if createErr != nil {
		failedToCreateUserSecretMsg := fmt.Sprintf("Failed to create secret %s", secret.Name)
		SetAccountStatus(reqLogger, account, failedToCreateUserSecretMsg, awsv1alpha1.AccountFailed, "Failed")
		err := r.Client.Status().Update(context.TODO(), account)
		if err != nil {
			return err
		}
		reqLogger.Info(failedToCreateUserSecretMsg)
		return createErr
	}
	reqLogger.Info(fmt.Sprintf("Created secret %s", secret.Name))
	return nil
}

// BuildSTSUser sets up an IAM user with the proper access and creates secrets to hold credentials
// Takes a logger, an awsSetupClient for the signing token, an awsClient for, an account CR to set ownership of secrets, the namespace to create the secret in, and a role to assume with the creds
// The awsSetupClient is the client for the user in the target linked account
// The awsClient is the client for the user in the payer level account
func (r *ReconcileAccount) BuildSTSUser(reqLogger logr.Logger, awsSetupClient awsclient.Client, awsClient awsclient.Client, account *awsv1alpha1.Account, nameSpace string, iamRole string) (string, error) {
	reqLogger.Info("Creating IAM STS User")

	// If IAM user was just created we cannot immediately create STS credentials due to an issue
	// with eventual consisency on AWS' side
	time.Sleep(10 * time.Second)

	// Create the temporary sre-admin user credentials using STS
	STSCredentials, STSCredentialsErr := getStsCredentials(reqLogger, awsClient, iamRole, account.Spec.AwsAccountID)
	if STSCredentialsErr != nil {
		reqLogger.Info("Failed to get SRE admin STSCredentials from AWS api ", "Error", STSCredentialsErr.Error())
		return "", STSCredentialsErr
	}

	STSUserName := account.Name + "-STS"

	IAMAdministratorPolicy := "arn:aws:iam::aws:policy/AdministratorAccess"

	IAMPolicy := sts.PolicyDescriptorType{Arn: &IAMAdministratorPolicy}

	IAMPolicyDescriptors := []*sts.PolicyDescriptorType{&IAMPolicy}

	SigninTokenDuration := int64(credentialwatcher.STSCredentialsDuration)

	// gets Web Console login, this policy cannot grant more permissions than the IAM user has which creates it
	// which is why we're using awsSetupClient here and not awsClient
	SREConsoleLoginURL, err := RequestSigninToken(reqLogger, awsSetupClient, &SigninTokenDuration, &STSUserName, IAMPolicyDescriptors, STSCredentials)
	if err != nil {
		reqLogger.Error(err, "Unable to create AWS signin token")
	}

	secretName := account.Name

	// Create Console Secret
	consoleSecretName := fmt.Sprintf("%s-sre-console-url", secretName)
	consoleSecretData := map[string][]byte{
		"aws_console_login_url": []byte(SREConsoleLoginURL),
	}
	userConsoleSecret := CreateSecret(consoleSecretName, nameSpace, consoleSecretData)
	err = r.CreateSecret(reqLogger, account, userConsoleSecret)
	if err != nil {
		return "", err
	}

	// Create sre-cli user secret
	cliSecretName := fmt.Sprintf("%s-sre-cli-credentials", secretName)
	cliSecretData := map[string][]byte{
		"aws_access_key_id":     []byte(*STSCredentials.Credentials.AccessKeyId),
		"aws_secret_access_key": []byte(*STSCredentials.Credentials.SecretAccessKey),
		"aws_session_token":     []byte(*STSCredentials.Credentials.SessionToken),
	}

	userSecret := CreateSecret(cliSecretName, nameSpace, cliSecretData)

	err = r.CreateSecret(reqLogger, account, userSecret)
	if err != nil {
		return "", err
	}

	return userSecret.ObjectMeta.Name, nil
}

// getStsCredentials returns STS credentials for the specified account ARN
// Takes a logger, an awsClient, a role name to assume, and the target AWS account ID
func getStsCredentials(reqLogger logr.Logger, client awsclient.Client, iamRoleName string, awsAccountID string) (*sts.AssumeRoleOutput, error) {
	// Use the role session name to uniquely identify a session when the same role
	// is assumed by different principals or for different reasons.
	var roleSessionName = "awsAccountOperator"
	// Default duration in seconds of the session token 3600. We need to have the roles policy
	// changed if we want it to be longer than 3600 seconds
	var roleSessionDuration int64 = 3600
	// The role ARN made up of the account number and the role which is the default role name
	// created in child accounts
	var roleArn = fmt.Sprintf("arn:aws:iam::%s:role/%s", awsAccountID, iamRoleName)
	reqLogger.Info(fmt.Sprintf("Creating STS credentials for AWS ARN: %s", roleArn))
	// Build input for AssumeRole
	assumeRoleInput := sts.AssumeRoleInput{
		DurationSeconds: &roleSessionDuration,
		RoleArn:         &roleArn,
		RoleSessionName: &roleSessionName,
	}

	assumeRoleOutput := &sts.AssumeRoleOutput{}
	var err error
	for i := 0; i < 100; i++ {
		time.Sleep(500 * time.Millisecond)
		assumeRoleOutput, err = client.AssumeRole(&assumeRoleInput)
		if err == nil {
			break
		}
		if i == 99 {
			reqLogger.Info(fmt.Sprintf("Timed out while assuming role %s", roleArn))
		}
	}
	if err != nil {
		// Log AWS error
		if aerr, ok := err.(awserr.Error); ok {
			reqLogger.Error(aerr,
				fmt.Sprintf(`New AWS Error while getting STS credentials,
					AWS Error Code: %s,
					AWS Error Message: %s`,
					aerr.Code(),
					aerr.Message()))
		}
		return &sts.AssumeRoleOutput{}, err
	}

	return assumeRoleOutput, nil
}

// formatFederatedCredentials returns a JSON byte array containing federation credentials
// Takes a logger, and the AWS output from a call to get a Federated Token
func formatFederatedCredentials(reqLogger logr.Logger, federatedTokenCredentials *sts.GetFederationTokenOutput) ([]byte, error) {
	var jsonCredentials []byte

	// Build JSON credentials for federation requests
	federationCredentials := map[string]string{
		"sessionId":    *federatedTokenCredentials.Credentials.AccessKeyId,
		"sessionKey":   *federatedTokenCredentials.Credentials.SecretAccessKey,
		"sessionToken": *federatedTokenCredentials.Credentials.SessionToken,
	}

	jsonCredentials, err := json.Marshal(federationCredentials)

	if err != nil {
		reqLogger.Error(err, "Error serializing federated URL as JSON")
		return jsonCredentials, err
	}

	return jsonCredentials, nil

}

// formatSiginTokenURL take STS credentials and build a URL for signing
// Takes a logger, a base URL for federation, and the required credentials for the session in a byte array of raw JSON
func formatSigninTokenURL(reqLogger logr.Logger, federationEndpointURL string, jsonFederatedCredentials []byte) (*url.URL, error) {
	// Build URL to request Signin Token via Federation end point
	baseFederationURL, err := url.Parse(federationEndpointURL)

	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Malformed URL: %s", err.Error()))
		return baseFederationURL, err
	}

	federationParams := url.Values{}

	federationParams.Add("Action", "getSigninToken")
	federationParams.Add("SessionType", "json")
	federationParams.Add("Session", string(jsonFederatedCredentials))

	baseFederationURL.RawQuery = federationParams.Encode()

	return baseFederationURL, nil

}

// requestSignedURL makes a HTTP call to the baseFederationURL to retrieve a signed federated URL for web console login
// Takes a logger, and the base URL
func requestSignedURL(reqLogger logr.Logger, baseFederationURL string) ([]byte, error) {
	// Make HTTP request to retrieve Federated Signin Token
	res, err := http.Get(baseFederationURL)

	if err != nil {
		getErrMsg := fmt.Sprintf("Error requesting Signin Token from: %s\n", baseFederationURL)
		reqLogger.Error(err, getErrMsg)
		return nil, err
	}

	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)

	if err != nil {
		bodyReadErrMsg := fmt.Sprintf("Unable to read response body: %s", baseFederationURL)
		reqLogger.Error(err, bodyReadErrMsg)
		return body, err
	}

	return body, nil
}

// getSigninToken makes a request to the federation endpoint to sign signin token
// Takes a logger, the base url, and the federation token to sign with
func getSigninToken(reqLogger logr.Logger, federationEndpointURL string, federatedTokenCredentials *sts.GetFederationTokenOutput) (awsSigninTokenResponse, error) {
	var signinResponse awsSigninTokenResponse

	jsonFederatedCredentials, err := formatFederatedCredentials(reqLogger, federatedTokenCredentials)

	if err != nil {
		return signinResponse, err
	}

	baseFederationURL, err := formatSigninTokenURL(reqLogger, federationEndpointURL, jsonFederatedCredentials)

	if err != nil {
		return signinResponse, err
	}

	signedFederatedURL, err := requestSignedURL(reqLogger, baseFederationURL.String())

	if err != nil {
		return signinResponse, err
	}

	// Unmarshal JSON response so we can extract the signin token
	err = json.Unmarshal(signedFederatedURL, &signinResponse)

	if err != nil {
		reqLogger.Error(err, "Error unmarshalling Federated Signin Response JSON")
		return signinResponse, err
	}

	return signinResponse, nil

}

// deleteAllAccessKeys deletes all access key pairs for a given user
// Takes a logger, an AWS client, and the target IAM user's username
func deleteAllAccessKeys(client awsclient.Client, iamUser *iam.User) error {
	accessKeyList, err := client.ListAccessKeys(&iam.ListAccessKeysInput{UserName: iamUser.UserName})
	if err != nil {
		return err
	}

	// Range through all AccessKeys for IAM user and delete them
	for index := range accessKeyList.AccessKeyMetadata {
		_, err = client.DeleteAccessKey(&iam.DeleteAccessKeyInput{AccessKeyId: accessKeyList.AccessKeyMetadata[index].AccessKeyId, UserName: iamUser.UserName})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkIAMUserExists checks if a given IAM user exists within an account
// Takes a logger, an AWS client for the target account, and a target IAM username
func checkIAMUserExists(reqLogger logr.Logger, client awsclient.Client, userName string) (bool, *iam.GetUserOutput, error) {
	// Retry when getting IAM user information
	// Sometimes we see a delay before credentials are ready to be user resulting in the AWS API returning 404's
	var iamGetUserOutput *iam.GetUserOutput
	var err error

	attempt := 1
	for i := 0; i < 10; i++ {
		// check if username exists for this account
		iamGetUserOutput, err = client.GetUser(&iam.GetUserInput{
			UserName: aws.String(userName),
		})

		attempt++
		// handle errors
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case iam.ErrCodeNoSuchEntityException:
					return false, nil, nil
				case "InvalidClientTokenId":
					invalidTokenMsg := fmt.Sprintf("Invalid Token error from AWS when attempting get IAM user %s, trying again", userName)
					reqLogger.Info(invalidTokenMsg)
					if attempt == 10 {
						return false, nil, err
					}
				case "AccessDenied":
					checkUserMsg := fmt.Sprintf("AWS Error while checking IAM user %s exists, trying again", userName)
					utils.LogAwsError(reqLogger, checkUserMsg, nil, err)
					// We may have bad credentials so return an error if so
					if attempt == 10 {
						return false, nil, err
					}
				default:
					utils.LogAwsError(reqLogger, "checkIAMUserExists: Unexpected AWS Error when checking IAM user exists", nil, err)
					return false, nil, err
				}
				time.Sleep(time.Duration(time.Duration(attempt*5) * time.Second))
			} else {
				return false, nil, fmt.Errorf("Unable to check if user %s exists error: %s", userName, err)
			}
		} else {
			// Break for loop if no errors present.
			break
		}
	}

	// User exists return
	return true, iamGetUserOutput, nil
}

// CreateIAMUser creates a new IAM user in the target AWS account
// Takes a logger, an AWS client for the target account, and the desired IAM username
func CreateIAMUser(reqLogger logr.Logger, client awsclient.Client, userName string) (*iam.CreateUserOutput, error) {
	var createUserOutput *iam.CreateUserOutput
	var err error

	attempt := 1
	for i := 0; i < 10; i++ {

		createUserOutput, err = client.CreateUser(&iam.CreateUserInput{
			UserName: aws.String(userName),
		})

		attempt++
		// handle errors
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				// Since we're using the same credentials to create the user as we did to check if the user exists
				// we can continue to try without returning, also the outer loop will eventually return
				case "InvalidClientTokenId":
					invalidTokenMsg := fmt.Sprintf("Invalid Token error from AWS when attempting to create user %s, trying again", userName)
					reqLogger.Info(invalidTokenMsg)
					if attempt == 10 {
						return &iam.CreateUserOutput{}, err
					}
				// createUserOutput inconsistently returns "InvalidClientTokenId" if that happens then the next call to
				// create the user will fail with EntitiyAlreadyExists. Since we verity the user doesn't exist before this
				// loop we can safely assume we created the user on our first loop.
				case iam.ErrCodeEntityAlreadyExistsException:
					invalidTokenMsg := fmt.Sprintf("IAM User %s was created", userName)
					reqLogger.Info(invalidTokenMsg)
					return &iam.CreateUserOutput{}, err
				default:
					utils.LogAwsError(reqLogger, "CreateIAMUser: Unexpected AWS Error during creation of IAM user", nil, err)
					return &iam.CreateUserOutput{}, err
				}
				time.Sleep(time.Duration(time.Duration(attempt*5) * time.Second))
			} else {
				return &iam.CreateUserOutput{}, err
			}
		} else {
			// Break for loop if no errors are present.
			break
		}
	}
	// User creation successful
	return createUserOutput, nil
}

// AttachAdminUserPolicy attaches the AdministratorAccess policy to a target user
// Takes a logger, an AWS client for the target account, and the target IAM user's username
func AttachAdminUserPolicy(client awsclient.Client, iamUser *iam.User) (*iam.AttachUserPolicyOutput, error) {

	attachPolicyOutput := &iam.AttachUserPolicyOutput{}
	var err error
	for i := 0; i < 100; i++ {
		time.Sleep(500 * time.Millisecond)
		attachPolicyOutput, err = client.AttachUserPolicy(&iam.AttachUserPolicyInput{
			UserName:  iamUser.UserName,
			PolicyArn: aws.String("arn:aws:iam::aws:policy/AdministratorAccess"),
		})
		if err == nil {
			break
		}
	}
	if err != nil {
		return &iam.AttachUserPolicyOutput{}, err
	}

	return attachPolicyOutput, nil
}

// CreateUserAccessKey creates a new IAM Access Key in AWS and returns aws.CreateAccessKeyOutput struct containing access key and secret
func CreateUserAccessKey(client awsclient.Client, iamUser *iam.User) (*iam.CreateAccessKeyOutput, error) {

	// Create new access key for user
	result, err := client.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: iamUser.UserName})
	if err != nil {
		return &iam.CreateAccessKeyOutput{}, err
	}

	return result, nil
}

// BuildIAMUser creates and initializes all resources needed for a new IAM user
// Takes a logger, an AWS client, an Account CR, the desired IAM username and a namespace to create resources in
func (r *ReconcileAccount) BuildIAMUser(reqLogger logr.Logger, awsClient awsclient.Client, account *awsv1alpha1.Account, iamUserName string, nameSpace string) (*string, error) {
	var iamUserSecretName string
	var createdIAMUser *iam.User

	// Check if IAM User exists for this account
	iamUserExists, iamUserExistsOutput, err := awsclient.CheckIAMUserExists(reqLogger, awsClient, iamUserName)
	if err != nil {
		return nil, err
	}

	// Create IAM user in AWS if it doesn't exist
	if iamUserExists {
		// If user exists extract iam.User pointer
		createdIAMUser = iamUserExistsOutput.User
	} else {
		CreateUserOutput, err := awsclient.CreateIAMUser(reqLogger, awsClient, account, iamUserName)
		// Err is handled within the function and returns a error message
		if err != nil {
			return nil, err
		}

		// Extract iam.User as pointer
		createdIAMUser = CreateUserOutput.User
	}

	// Determine the kubernetes secret name as its different if the IAM user is osdManagedAdminSRE
	if isIAMUserOsdManagedAdminSRE(createdIAMUser.UserName) {
		// Use iamUserNameSRE constant here to ensure we don't double up on suffix for secret name
		iamUserSecretName = createIAMUserSecretName(fmt.Sprintf("%s-%s", account.Name, iamUserNameSRE))
	} else {
		iamUserSecretName = createIAMUserSecretName(account.Name)
	}

	reqLogger.Info(fmt.Sprintf("Attaching Admin Policy to IAM user %s", aws.StringValue(createdIAMUser.UserName)))

	// Setting IAM user policy
	_, err = AttachAdminUserPolicy(awsClient, createdIAMUser)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to attach admin policy to IAM user %s", aws.StringValue(createdIAMUser.UserName))
		reqLogger.Error(err, errMsg)
		return nil, err
	}

	reqLogger.Info(fmt.Sprintf("Creating Secrets for IAM user %s", aws.StringValue(createdIAMUser.UserName)))

	// Create a NamespacedName for the secret
	secretNamespacedName := types.NamespacedName{Name: iamUserSecretName, Namespace: nameSpace}

	secretExists, err := r.DoesSecretExist(secretNamespacedName)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Unable check if secret: %s exists", secretNamespacedName.String()))
		return nil, err
	}

	if !secretExists {
		iamAccessKeyOutput, err := r.RotateIAMAccessKeys(reqLogger, awsClient, account, createdIAMUser)
		if err != nil {
			errMsg := fmt.Sprintf("Unable to rotate access keys for IAM user: %s", aws.StringValue(createdIAMUser.UserName))
			reqLogger.Error(err, errMsg)
			return nil, err
		}

		err = r.createIAMUserSecret(reqLogger, account, secretNamespacedName, iamAccessKeyOutput)
		if err != nil {
			errMsg := fmt.Sprintf("Unable to create secret: %s", secretNamespacedName.Name)
			reqLogger.Error(err, errMsg)
			return nil, err
		}
	}

	// Return secret name
	return &iamUserSecretName, nil
}

// RotateIAMAccessKeys will delete all AWS access keys assigned to the user and recreate them
func (r *ReconcileAccount) RotateIAMAccessKeys(reqLogger logr.Logger, awsClient awsclient.Client, account *awsv1alpha1.Account, iamUser *iam.User) (*iam.CreateAccessKeyOutput, error) {

	// Delete all current access keys
	err := deleteAllAccessKeys(awsClient, iamUser)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Failed to delete IAM access keys for %s", aws.StringValue(iamUser.UserName)))
		return nil, err
	}
	// Create new access key
	accessKeyOutput, err := CreateUserAccessKey(awsClient, iamUser)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to create IAM access key for IAM user %s", aws.StringValue(iamUser.UserName))
		reqLogger.Error(err, errMsg)
		// TODO: We should move this status update to the main reconcile where BuildIAMUser is called
		// This would mean we can remove reqLogger and the awsv1alpha1 account reference to keep things cleaner
		SetAccountStatus(reqLogger, account, errMsg, awsv1alpha1.AccountFailed, AccountFailed)
		err := r.Client.Status().Update(context.TODO(), account)
		if err != nil {
			return nil, err
		}
		return nil, err
	}

	return accessKeyOutput, nil
}

// createIAMUserSecret creates a K8s secret from iam.createAccessKeyOuput and sets the owner reference to the controller
func (r *ReconcileAccount) createIAMUserSecret(reqLogger logr.Logger, account *awsv1alpha1.Account, secretName types.NamespacedName, createAccessKeyOutput *iam.CreateAccessKeyOutput) error {

	// Fill in the secret data
	userSecretData := map[string][]byte{
		"aws_user_name":         []byte(*createAccessKeyOutput.AccessKey.UserName),
		"aws_access_key_id":     []byte(*createAccessKeyOutput.AccessKey.AccessKeyId),
		"aws_secret_access_key": []byte(*createAccessKeyOutput.AccessKey.SecretAccessKey),
	}

	// Create new secret
	iamUserSecret := CreateSecret(secretName.Name, secretName.Namespace, userSecretData)

	// Set controller as owner of secret
	if err := controllerutil.SetControllerReference(account, iamUserSecret, r.scheme); err != nil {
		return err
	}

	// Return nil or err if we're unable to create the k8s secret
	return r.CreateSecret(reqLogger, account, iamUserSecret)
}

// DoesSecretExist checks to see if a given secret exists
func (r *ReconcileAccount) DoesSecretExist(namespacedName types.NamespacedName) (bool, error) {

	secret := &corev1.Secret{}
	err := r.Client.Get(context.TODO(), namespacedName, secret)
	if err != nil {
		if k8serr.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// isIAMUserOsdManagedAdminSRE returns true if the username begins with osdManagedAdminSRE
func isIAMUserOsdManagedAdminSRE(userName *string) bool {
	return strings.HasPrefix(*userName, iamUserNameSRE)
}

// createIAMUserSecretName returns a lower case concatinated string of the input separated by "-"
func createIAMUserSecretName(account string) string {
	suffix := "secret"
	return strings.ToLower(fmt.Sprintf("%s-%s", account, suffix))
}
