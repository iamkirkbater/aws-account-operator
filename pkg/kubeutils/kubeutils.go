package kubeutils

import (
	awsv1alpha1 "github.com/openshift/aws-account-operator/pkg/apis/aws/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetConfigMap returns the default AWS Account Operator ConfigMap
func GetConfigMap(kubeClient client.Client) (corev1.ConfigMap, error) {

}
