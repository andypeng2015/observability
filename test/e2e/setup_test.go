// +build e2e

/*
Copyright 2018 The Knative Authors

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

package e2e

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/knative/observability/pkg/apis/sink/v1alpha1"
	v1alpha1i "github.com/knative/observability/pkg/client/clientset/versioned/typed/sink/v1alpha1"
	"github.com/knative/pkg/test"
	"github.com/knative/pkg/test/logging"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kuberrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/clientcmd"

	oversioned "github.com/knative/observability/pkg/client/clientset/versioned"
)

// TestMain is called by the test binary generated by "go test", and is
// responsible for setting up and tearing down the testing environment, namely
// the test namespace.
func TestMain(m *testing.M) {
	flag.Parse()
	logging.InitializeLogger(test.Flags.LogVerbose)
	logger := logging.GetContextLogger("TestSetup")
	flag.Set("alsologtostderr", "true")
	if test.Flags.EmitMetrics {
		logging.InitializeMetricExporter()
	}

	clients := setup(logger)
	test.CleanupOnInterrupt(func() {
		teardownNamespace(clients, logger)
	}, logger)
	var code int

	defer func() {
		// Cleanup namespace
		if code == 0 {
			teardownNamespace(clients, logger)
		}
		// Exit with m.Run exit code
		os.Exit(code)
	}()

	code = m.Run()
}

type clients struct {
	kubeClient *test.KubeClient
	sinkClient sinkClient
}

const observabilityTestNamespace = "observability-tests"

func teardownNamespace(clients *clients, logger *logging.BaseLogger) {
	logger.Infof("Deleting namespace %q", observabilityTestNamespace)

	err := clients.kubeClient.Kube.CoreV1().Namespaces().Delete(
		observabilityTestNamespace,
		&metav1.DeleteOptions{},
	)
	if err != nil && !kuberrors.IsNotFound(err) {
		logger.Fatalf("Error deleting namespace %q: %v", observabilityTestNamespace, err)
	}
}

func clusterNodes(client *test.KubeClient) (*corev1.NodeList, error) {
	return client.Kube.CoreV1().Nodes().List(metav1.ListOptions{})
}

func setup(logger *logging.BaseLogger) *clients {
	clients, err := newClients()
	if err != nil {
		logger.Fatalf("Error creating newClients: %v", err)
	}

	// Ensure the test namespace exists, by trying to create it and ignoring
	// already-exists errors.
	if _, err := clients.kubeClient.Kube.CoreV1().Namespaces().Create(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: observabilityTestNamespace,
			},
		},
	); err == nil {
		logger.Infof("Created namespace %q", observabilityTestNamespace)
	} else if kuberrors.IsAlreadyExists(err) {
		logger.Infof("Namespace %q already exists", observabilityTestNamespace)
	} else {
		logger.Fatalf("Error creating namespace %q: %v", observabilityTestNamespace, err)
	}
	return clients
}

func newClients() (*clients, error) {
	configPath := test.Flags.Kubeconfig
	namespace := observabilityTestNamespace
	clusterName := test.Flags.Cluster

	overrides := clientcmd.ConfigOverrides{}
	// Override the cluster name if provided.
	if clusterName != "" {
		overrides.Context.Cluster = clusterName
	}

	kubeClient, err := test.NewKubeClient(configPath, clusterName)
	if err != nil {
		return nil, err
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: configPath,
		},
		&overrides,
	).ClientConfig()
	if err != nil {
		return nil, err
	}

	sc, err := oversioned.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &clients{
		kubeClient: kubeClient,
		sinkClient: sinkClient{
			LogSink:        sc.Observability().LogSinks(namespace),
			ClusterLogSink: sc.Observability().ClusterLogSinks(namespace),
		},
	}, nil
}

type sinkClient struct {
	LogSink        v1alpha1i.LogSinkInterface
	ClusterLogSink v1alpha1i.ClusterLogSinkInterface
}

func assertErr(t *testing.T, msg string, err error) {
	if err != nil {
		t.Fatalf(msg, err)
	}
}

func createLogSink(
	t *testing.T,
	logger *logging.BaseLogger,
	prefix string,
	sc sinkClient,
) {
	logger.Info("Creating the log sink")
	_, err := sc.LogSink.Create(&v1alpha1.LogSink{
		ObjectMeta: metav1.ObjectMeta{
			Name: prefix + "test",
		},
		Spec: v1alpha1.SinkSpec{
			Type: "syslog",
			Host: prefix + "syslog-receiver." + observabilityTestNamespace,
			Port: 24903,
		},
	})
	assertErr(t, "Error creating LogSink: %v", err)
}

func createSyslogReceiver(
	t *testing.T,
	logger *logging.BaseLogger,
	prefix string,
	kc *test.KubeClient,
) {
	logger.Info("Creating the service for the syslog receiver")
	_, err := kc.Kube.Core().Services(observabilityTestNamespace).Create(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: prefix + "syslog-receiver",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name: "syslog",
				Port: 24903,
			}, {
				Name: "metrics",
				Port: 6060,
			}},
			Selector: map[string]string{
				"app": prefix + "syslog-receiver",
			},
		},
	})
	assertErr(t, "Error creating Syslog Receiver Service: %v", err)

	logger.Info("Creating the pod for the syslog receiver")
	_, err = kc.Kube.Core().Pods(observabilityTestNamespace).Create(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: prefix + "syslog-receiver",
			Labels: map[string]string{
				"app": prefix + "syslog-receiver",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "syslog-receiver",
				Image: "oratos/crosstalk-receiver:v0.3",
				Ports: []corev1.ContainerPort{{
					Name:          "syslog-port",
					ContainerPort: 24903,
				}, {
					Name:          "metrics-port",
					ContainerPort: 6060,
				}},
				Env: []corev1.EnvVar{{
					Name:  "SYSLOG_PORT",
					Value: "24903",
				}, {
					Name:  "METRICS_PORT",
					Value: "6060",
				}, {
					Name:  "MESSAGE",
					Value: prefix + "test-log-message",
				}},
			}},
		},
	})
	assertErr(t, "Error creating Syslog Receiver: %v", err)

	logger.Info("Waiting for syslog receiver to be running")
	syslogState := func(ps *corev1.PodList) (bool, error) {
		for _, p := range ps.Items {
			if p.Labels["app"] == prefix+"syslog-receiver" && p.Status.Phase == corev1.PodRunning {
				return true, nil
			}
		}
		return false, nil
	}
	err = test.WaitForPodListState(
		kc,
		syslogState,
		prefix+"syslog-receiver",
		observabilityTestNamespace,
	)
	assertErr(t, "Error waiting for syslog-receiver to be running: %v", err)
}

func waitForFluentBitToBeReady(
	t *testing.T,
	logger *logging.BaseLogger,
	prefix string,
	kc *test.KubeClient,
) {
	logger.Info("Giving sink-controller time to delete fluentbit pods")
	time.Sleep(5 * time.Second)

	logger.Info("Getting cluster nodes")
	nodes, err := clusterNodes(kc)
	assertErr(t, "Error getting the cluster nodes: %v", err)

	logger.Info("Waiting for all fluentbit pods to be ready")
	fluentState := func(ps *corev1.PodList) (bool, error) {
		var readyCount int
		for _, p := range ps.Items {
			if p.Labels["app"] == "fluent-bit-ds" && ready(p) {
				readyCount++
			}
		}
		return readyCount == len(nodes.Items), nil
	}
	err = test.WaitForPodListState(
		kc,
		fluentState,
		prefix+"fluent",
		"knative-observability",
	)
	assertErr(t, "Error waiting for fluent-bit to be ready: %v", err)
}

func ready(p corev1.Pod) bool {
	if len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, s := range p.Status.ContainerStatuses {
		if !s.Ready {
			return false
		}
	}
	return true
}

func assertTheLogsGotThere(
	t *testing.T,
	logger *logging.BaseLogger,
	prefix string,
	kc *test.KubeClient,
) {
	logger.Info("Get the count for number of logs received")
	_, err := kc.Kube.Batch().Jobs(observabilityTestNamespace).Create(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: prefix + "log-observer",
			Labels: map[string]string{
				"app": prefix + "log-observer",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": prefix + "log-observer",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "log-observer",
						Image: "oratos/ci-base",
						Command: []string{
							"bash",
							"-c",
							fmt.Sprintf(`
								for _ in {1..10}; do
									LOG_COUNT=$(curl -s %ssyslog-receiver.observability-tests:6060/metrics | jq -r '.cluster')
									echo "Logs Received: $LOG_COUNT"
									sleep 1
								done
							`, prefix),
						},
					}},
				},
			},
		},
	})
	assertErr(t, "Error creating log-observer: %v", err)

	logger.Info("Waiting for log-observer job to be completed")
	var logObserverPodName string
	logObserverState := func(ps *corev1.PodList) (bool, error) {
		for _, p := range ps.Items {
			if p.Labels["app"] == prefix+"log-observer" && p.Status.Phase == corev1.PodSucceeded {
				logObserverPodName = p.Name
				return true, nil
			}
		}
		return false, nil
	}
	err = test.WaitForPodListState(
		kc,
		logObserverState,
		prefix+"log-observer",
		observabilityTestNamespace,
	)
	assertErr(t, "Error waiting for log-observer to be completed: %v", err)

	req := kc.Kube.Core().Pods(observabilityTestNamespace).GetLogs(
		logObserverPodName,
		&corev1.PodLogOptions{},
	)

	b, err := req.Do().Raw()
	assertErr(t, "Error reading logs from the log-observer: %v", err)

	if !strings.Contains(string(b), "Logs Received: 10") {
		t.Fatalf("Received log count is not 10: \n%s\n", string(b))
	}
}

func emitLogs(
	t *testing.T,
	logger *logging.BaseLogger,
	prefix string,
	kc *test.KubeClient,
) {
	logger.Info("Emitting logs")
	_, err := kc.Kube.Batch().Jobs(observabilityTestNamespace).Create(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: prefix + "log-emitter",
			Labels: map[string]string{
				"app": prefix + "log-emitter",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": prefix + "log-emitter",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "log-emitter",
						Image: "ubuntu:xenial",
						Command: []string{
							"bash",
							"-c",
							fmt.Sprintf("for _ in {1..10}; do echo %stest-log-message; sleep 0.5; done", prefix),
						},
					}},
				},
			},
		},
	})
	assertErr(t, "Error creating log-emitter: %v", err)

	logger.Info("Waiting for log-emitter job to be completed")
	logEmitterState := func(ps *corev1.PodList) (bool, error) {
		for _, p := range ps.Items {
			if p.Labels["app"] == prefix+"log-emitter" && p.Status.Phase == corev1.PodSucceeded {
				return true, nil
			}
		}
		return false, nil
	}
	err = test.WaitForPodListState(
		kc,
		logEmitterState,
		prefix+"log-emitter",
		observabilityTestNamespace,
	)
	assertErr(t, "Error waiting for log-emitter to be completed: %v", err)
}