// +build e2e

/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"testing"
	"text/template"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	"github.com/onsi/ginkgo/reporters"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/session"
	cfn "github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	awssts "github.com/aws/aws-sdk-go/service/sts"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/cloudformation"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/sts"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha3"
	common "sigs.k8s.io/cluster-api/test/helpers/components"
	capiFlag "sigs.k8s.io/cluster-api/test/helpers/flag"
	"sigs.k8s.io/cluster-api/test/helpers/kind"
	"sigs.k8s.io/cluster-api/test/helpers/scheme"
	"sigs.k8s.io/cluster-api/util"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestE2e(t *testing.T) {
	RegisterFailHandler(Fail)

	// If running in prow, output the junit files to the artifacts path
	junitPath := fmt.Sprintf("junit.e2e_suite.%d.xml", config.GinkgoConfig.ParallelNode)
	artifactPath, exists := os.LookupEnv("ARTIFACTS")
	if exists {
		junitPath = path.Join(artifactPath, junitPath)
	}
	junitReporter := reporters.NewJUnitReporter(junitPath)
	RunSpecsWithDefaultAndCustomReporters(t, "e2e Suite", []Reporter{junitReporter})
}

const (
	capiNamespace      = "capi-system"
	capiDeploymentName = "capi-controller-manager"
	capaNamespace      = "capa-system"
	capaDeploymentName = "capa-controller-manager"
	setupTimeout       = 10 * 60
	stackName          = "cluster-api-provider-aws-sigs-k8s-io"
	keyPairName        = "cluster-api-provider-aws-sigs-k8s-io"
)

var (
	managerImage    = capiFlag.DefineOrLookupStringFlag("managerImage", "", "Docker image to load into the kind cluster for testing")
	capaComponents  = capiFlag.DefineOrLookupStringFlag("capaComponents", "", "capa components to load")
	kustomizeBinary = capiFlag.DefineOrLookupStringFlag("kustomizeBinary", "kustomize", "path to the kustomize binary")

	kindCluster  kind.Cluster
	kindClient   crclient.Client
	clientSet    *kubernetes.Clientset
	sess         client.ConfigProvider
	accountID    string
	accessKey    *iam.AccessKey
	suiteTmpDir  string
	region       string
	artifactPath string
	logPath      string
)

var _ = BeforeSuite(func() {
	artifactPath, _ = os.LookupEnv("ARTIFACTS")
	logPath = path.Join(artifactPath, "logs")
	Expect(os.MkdirAll(filepath.Dir(logPath), 0755)).To(Succeed())

	fmt.Fprintf(GinkgoWriter, "Setting up kind cluster\n")

	var err error
	suiteTmpDir, err = ioutil.TempDir("", "capa-e2e-suite")
	Expect(err).NotTo(HaveOccurred())

	var ok bool
	region, ok = os.LookupEnv("AWS_REGION")
	fmt.Fprintf(GinkgoWriter, "Running in region: %s\n", region)
	if !ok {
		fmt.Fprintf(GinkgoWriter, "Environment variable AWS_REGION not found")
		Expect(ok).To(BeTrue())
	}

	sess = getSession()

	fmt.Fprintf(GinkgoWriter, "Creating AWS prerequisites\n")
	accountID = getAccountID(sess)
	createKeyPair(sess)
	createIAMRoles(sess, accountID)

	iamc := iam.New(sess)
	out, err := iamc.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String("bootstrapper.cluster-api-provider-aws.sigs.k8s.io")})
	Expect(err).NotTo(HaveOccurred())
	Expect(out.AccessKey).NotTo(BeNil())
	accessKey = out.AccessKey

	kindCluster = kind.Cluster{
		Name: "capa-test-" + util.RandomString(6),
	}
	kindCluster.Setup()
	loadManagerImage(kindCluster)

	// create the management cluster clients we'll need
	restConfig := kindCluster.RestConfig()
	mapper, err := apiutil.NewDynamicRESTMapper(restConfig, apiutil.WithLazyDiscovery)
	Expect(err).NotTo(HaveOccurred())
	kindClient, err = crclient.New(kindCluster.RestConfig(), crclient.Options{Scheme: setupScheme(), Mapper: mapper})
	Expect(err).NotTo(HaveOccurred())
	clientSet, err = kubernetes.NewForConfig(kindCluster.RestConfig())
	Expect(err).NotTo(HaveOccurred())

	// Deploy CertManager
	certmanagerYaml := "https://github.com/jetstack/cert-manager/releases/download/v0.11.0/cert-manager.yaml"
	applyManifests(kindCluster, &certmanagerYaml)

	// Wait for CertManager to be available before continuing
	common.WaitDeployment(kindClient, "cert-manager", "cert-manager-webhook")

	// Deploy the CAPI components
	// workaround since there isn't a v1alpha3 capi release yet
	deployCAPIComponents(kindCluster)

	// Deploy the CAPA components
	deployCAPAComponents(kindCluster)

	// Verify capi components are deployed
	common.WaitDeployment(kindClient, capiNamespace, capiDeploymentName)
	go func() {
		defer GinkgoRecover()
		watchLogs(capiNamespace, capiDeploymentName, logPath)
	}()

	// Verify capa components are deployed
	common.WaitDeployment(kindClient, capaNamespace, capaDeploymentName)
	go func() {
		defer GinkgoRecover()
		watchLogs(capaNamespace, capaDeploymentName, logPath)
	}()

}, setupTimeout)

var _ = AfterSuite(func() {
	fmt.Fprintf(GinkgoWriter, "Tearing down kind cluster\n")

	if kindCluster.Name != "" {
		kindCluster.Teardown()
	}

	if reflect.TypeOf(sess) != nil {
		if accessKey != nil {
			iamc := iam.New(sess)
			iamc.DeleteAccessKey(&iam.DeleteAccessKeyInput{UserName: accessKey.UserName, AccessKeyId: accessKey.AccessKeyId})
		}
		deleteIAMRoles(sess)
	}

	if suiteTmpDir != "" {
		os.RemoveAll(suiteTmpDir)
	}
})

func watchLogs(namespace, deploymentName, logDir string) {
	deployment := &appsv1.Deployment{}
	Expect(kindClient.Get(context.TODO(), crclient.ObjectKey{Namespace: namespace, Name: deploymentName}, deployment)).To(Succeed())

	selector, err := metav1.LabelSelectorAsMap(deployment.Spec.Selector)
	Expect(err).NotTo(HaveOccurred())

	pods := &corev1.PodList{}
	Expect(kindClient.List(context.TODO(), pods, crclient.InNamespace(namespace), crclient.MatchingLabels(selector))).To(Succeed())

	for _, pod := range pods.Items {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			logFile := path.Join(logDir, deploymentName, pod.Name, container.Name+".log")
			fmt.Fprintf(GinkgoWriter, "Creating directory: %s\n", filepath.Dir(logFile))
			Expect(os.MkdirAll(filepath.Dir(logFile), 0755)).To(Succeed())

			opts := &corev1.PodLogOptions{
				Container: container.Name,
				Follow:    true,
			}

			podLogs, err := clientSet.CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream()
			Expect(err).NotTo(HaveOccurred())
			defer podLogs.Close()

			f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			Expect(err).NotTo(HaveOccurred())
			defer f.Close()

			out := bufio.NewWriter(f)
			defer out.Flush()
			_, err = out.ReadFrom(podLogs)
			if err != nil && err.Error() != "unexpected EOF" {
				Expect(err).NotTo(HaveOccurred())
			}
		}
	}
}

func getSession() client.ConfigProvider {
	sess, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	})
	Expect(err).NotTo(HaveOccurred())
	return sess
}

func getAccountID(prov client.ConfigProvider) string {
	stsSvc := sts.NewService(awssts.New(prov))
	accountID, err := stsSvc.AccountID()
	Expect(err).NotTo(HaveOccurred())
	return accountID
}

func createIAMRoles(prov client.ConfigProvider, accountID string) {
	cfnSvc := cloudformation.NewService(cfn.New(prov))
	Expect(
		cfnSvc.ReconcileBootstrapStack(stackName, accountID, "aws", []string{}, []string{}),
	).To(Succeed())
}

func deleteIAMRoles(prov client.ConfigProvider) {
	cfnSvc := cloudformation.NewService(cfn.New(prov))
	Expect(
		cfnSvc.DeleteStack(stackName),
	).To(Succeed())
}

func createKeyPair(prov client.ConfigProvider) {
	ec2c := ec2.New(prov)
	_, err := ec2c.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(keyPairName)})
	if code, _ := awserrors.Code(err); code != "InvalidKeyPair.Duplicate" {
		Expect(err).NotTo(HaveOccurred())
	}
}

func loadManagerImage(kindCluster kind.Cluster) {
	if managerImage != nil && *managerImage != "" {
		kindCluster.LoadImage(*managerImage)
	}
}

func applyManifests(kindCluster kind.Cluster, manifests *string) {
	Expect(manifests).ToNot(BeNil())
	fmt.Fprintf(GinkgoWriter, "Applying manifests for %s\n", *manifests)
	Expect(*manifests).ToNot(BeEmpty())
	kindCluster.ApplyYAML(*manifests)
}

func deployCAPIComponents(kindCluster kind.Cluster) {
	fmt.Fprintf(GinkgoWriter, "Generating CAPI manifests\n")

	// Build the manifests using kustomize
	capiManifests, err := exec.Command(*kustomizeBinary, "build", "https://github.com/kubernetes-sigs/cluster-api//config/default").Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(GinkgoWriter, "Error: %s\n", string(exitError.Stderr))
		}
	}
	Expect(err).NotTo(HaveOccurred())

	// write out the manifests
	manifestFile := path.Join(suiteTmpDir, "cluster-api-components.yaml")
	Expect(ioutil.WriteFile(manifestFile, capiManifests, 0644)).To(Succeed())

	// apply generated manifests
	applyManifests(kindCluster, &manifestFile)
}

func deployCAPAComponents(kindCluster kind.Cluster) {
	if capaComponents != nil && *capaComponents != "" {
		applyManifests(kindCluster, capaComponents)
		return
	}

	fmt.Fprintf(GinkgoWriter, "Generating CAPA manifests\n")

	// Build the manifests using kustomize
	capaManifests, err := exec.Command(*kustomizeBinary, "build", "../../config/default").Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(GinkgoWriter, "Error: %s\n", string(exitError.Stderr))
		}
	}
	Expect(err).NotTo(HaveOccurred())

	// envsubst the credentials
	Expect(err).NotTo(HaveOccurred())
	b64credentials := generateB64Credentials()
	os.Setenv("AWS_B64ENCODED_CREDENTIALS", b64credentials)
	manifestsContent := os.ExpandEnv(string(capaManifests))

	// write out the manifests
	manifestFile := path.Join(suiteTmpDir, "infrastructure-components.yaml")
	Expect(ioutil.WriteFile(manifestFile, []byte(manifestsContent), 0644)).To(Succeed())

	// apply generated manifests
	applyManifests(kindCluster, &manifestFile)
}

const AWSCredentialsTemplate = `[default]
aws_access_key_id = {{ .AccessKeyID }}
aws_secret_access_key = {{ .SecretAccessKey }}
region = {{ .Region }}
`

type awsCredential struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

func generateB64Credentials() string {
	creds := awsCredential{
		Region:          region,
		AccessKeyID:     *accessKey.AccessKeyId,
		SecretAccessKey: *accessKey.SecretAccessKey,
	}

	tmpl, err := template.New("AWS Credentials").Parse(AWSCredentialsTemplate)
	Expect(err).NotTo(HaveOccurred())

	var profile bytes.Buffer
	Expect(tmpl.Execute(&profile, creds)).To(Succeed())

	encCreds := base64.StdEncoding.EncodeToString(profile.Bytes())
	return encCreds
}

func setupScheme() *runtime.Scheme {
	s := scheme.SetupScheme()
	Expect(bootstrapv1.AddToScheme(s)).To(Succeed())
	Expect(infrav1.AddToScheme(s)).To(Succeed())
	return s
}
