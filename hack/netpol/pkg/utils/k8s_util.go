package utils

import (

	//	"context"
	"bytes"
	"fmt"
	"k8s.io/client-go/rest"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	v1net "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

type Kubernetes struct {
	podCache  map[string][]v1.Pod
	ClientSet *kubernetes.Clientset
}

func NewKubernetes() (*Kubernetes, error) {
	clientSet, err := Client()
	if err != nil {
		return nil, errors.WithMessagef(err, "unable to instantiate kube client")
	}
	return &Kubernetes{
		podCache:  map[string][]v1.Pod{},
		ClientSet: clientSet,
	}, nil
}

// GetPod returns a pod with the matching namespace and name
func (k *Kubernetes) GetPod(ns string, name string) (*v1.Pod, error) {
	pods, err := k.getPodsUncached(ns, "pod", name)
	if err != nil {
		return nil, errors.WithMessagef(err, "unable to get pod %s/%s", ns, name)
	}
	if len(pods) == 0 {
		return nil, nil
	}
	return &pods[0], nil
}

func (k *Kubernetes) getPodsUncached(ns string, key, val string) ([]v1.Pod, error) {
	v1PodList, err := k.ClientSet.CoreV1().Pods(ns).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%v=%v", key, val),
	})
	if err != nil {
		return nil, errors.WithMessage(err, "unable to list pods")
	}
	return v1PodList.Items, nil
}

// GetPods returns an array of all pods in the given namespace having a k/v label pair.
func (k *Kubernetes) GetPods(ns string, key string, val string) ([]v1.Pod, error) {
	if p, ok := k.podCache[fmt.Sprintf("%v_%v_%v", ns, key, val)]; ok {
		return p, nil
	}

	v1PodList, err := k.getPodsUncached(ns, key, val)
	if err != nil {
		return nil, errors.WithMessage(err, "unable to list pods")
	}
	k.podCache[fmt.Sprintf("%v_%v_%v", ns, key, val)] = v1PodList
	return v1PodList, nil
}

// Probe is execs into a pod and checks its connectivity to another pod.  Of course it assumes
// that the target pod is serving on the input port, and also that wget is installed.  For perf it uses
// spider rather then actually getting the full contents.
func (k *Kubernetes) Probe(ns1, pod1, ns2, pod2 string, port int) (bool, error) {
	fromPods, err := k.GetPods(ns1, "pod", pod1)
	if err != nil {
		return false, errors.WithMessagef(err, "unable to get pods from ns %s", ns1)
	}
	if len(fromPods) == 0 {
		return false, errors.New(fmt.Sprintf("no pod of name %s in namespace %s found", pod1, ns1))
	}
	fromPod := fromPods[0]

	toPods, err := k.GetPods(ns2, "pod", pod2)
	if err != nil {
		return false, errors.WithMessagef(err, "unable to get pods from ns %s", ns2)
	}
	if len(toPods) == 0 {
		return false, errors.New(fmt.Sprintf("no pod of name %s in namespace %s found", pod2, ns2))
	}
	toPod := toPods[0]

	toIP := toPod.Status.PodIP

	// note some versions of wget want -s for spider mode, others, -S
	exec := []string{"wget", "--spider", "--tries", "1", "--timeout", "1", "http://" + toIP + ":" + fmt.Sprintf("%v", port)}
	// HACK: inferring container name as c80, c81, etc, for simplicity.
	containerName := fmt.Sprintf("c%v", port)
	log.Info("Running: kubectl exec -t -i " + fromPod.Name + " -c " + containerName + " -n " + fromPod.Namespace + " -- " + strings.Join(exec, " "))
	out, out2, err := k.ExecuteRemoteCommand(fromPod, containerName, exec)
	log.Debug(".... Done")
	if err != nil {
		// log this error as debug since it's an expected failure
		log.Debugf("failed connect.... %v %v %v %v %v %v", out, out2, ns1, pod1, ns2, pod2)
		return false, errors.WithMessagef(err, "unable to execute remote command %+v", exec)
	}
	return true, nil
}

// ExecuteRemoteCommand executes a remote shell command on the given pod
// returns the output from stdout and stderr
func (k *Kubernetes) ExecuteRemoteCommand(pod v1.Pod, cname string, command []string) (string, string, error) {
	kubeCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	restCfg, err := kubeCfg.ClientConfig()
	if err != nil {
		return "", "", errors.WithMessagef(err, "unable to get rest config from kube config")
	}
	buf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	request := k.ClientSet.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").
		Name(pod.Name).SubResource("exec").VersionedParams(&v1.PodExecOptions{
		Container: cname,
		Command:   command,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       true},
		scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", request.URL())
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: buf,
		Stderr: errBuf,
	})
	if err != nil {
		return buf.String(), errBuf.String(), errors.Wrapf(err, "Failed executing command %s on %v/%v------/%v/%v", command, pod.Namespace, pod.Name, buf.String(), errBuf.String())
	}
	return buf.String(), errBuf.String(), nil
}

func Client() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := filepath.Join(
			os.Getenv("HOME"), ".kube", "config",
		)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, errors.WithMessagef(err, "unable to build config from flags, check that your KUBECONFIG file is correct !")
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.WithMessagef(err, "unable to instantiate clientset")
	}
	return clientset, nil
}

// CreateOrUpdateNamespace is a convenience function for idempotent setup of namespaces
func (k *Kubernetes) CreateOrUpdateNamespace(n string, labels map[string]string) (*v1.Namespace, error) {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   n,
			Labels: labels,
		},
	}
	nsr, err := k.ClientSet.CoreV1().Namespaces().Create(ns)
	if err == nil {
		log.Infof("created namespace %s", n)
		return nsr, nil
	}

	log.Debugf("unable to create namespace %s, let's try updating it instead (error: %s)", ns.Name, err)
	nsr, err = k.ClientSet.CoreV1().Namespaces().Update(ns)
	if err != nil {
		log.Debugf("unable to update namespace %s: %s", ns, err)
	}

	return nsr, err
}

// CreateOrUpdateDeployment is a convenience function for idempotent setup of deployments
func (k *Kubernetes) CreateOrUpdateDeployment(ns, deploymentName string, replicas int32, labels map[string]string) (*appsv1.Deployment, error) {
	zero := int64(0)
	log.Infof("creating/updating deployment %s in ns %s", deploymentName, ns)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Labels:    labels,
			Namespace: ns,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    labels,
					Namespace: ns,
				},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: &zero,
					Containers: []v1.Container{
						{
							Name:            "c80",
							Image:           "python:latest",
							Command:         []string{"python", "-m", "http.server", "80"},
							SecurityContext: &v1.SecurityContext{},
							Ports: []v1.ContainerPort{
								v1.ContainerPort{
									ContainerPort: 80,
									Name:          "serve-80",
								},
							},
						},
						{
							Name:            "c81",
							Image:           "python:latest",
							Command:         []string{"python", "-m", "http.server", "81"},
							SecurityContext: &v1.SecurityContext{},
							Ports: []v1.ContainerPort{
								v1.ContainerPort{
									ContainerPort: 81,
									Name:          "serve-81",
								},
							},
						},
					},
				},
			},
		},
	}

	d, err := k.ClientSet.AppsV1().Deployments(ns).Create(deployment)
	if err == nil {
		log.Infof("created deployment %s in namespace %s", d.Name, ns)
		return d, nil
	}

	log.Debugf("unable to create deployment %s in ns %s, let's try update instead", deployment.Name, ns)
	d, err = k.ClientSet.AppsV1().Deployments(ns).Update(d)
	if err != nil {
		log.Debugf("unable to update deployment %s in ns %s: %s", deployment.Name, ns, err)
	}
	return d, err
}

// CleanNetworkPolicies is a convenience function for deleting network policies before startup of any new test.
func (k *Kubernetes) CleanNetworkPolicies(namespaces []string) error {
	for _, ns := range namespaces {
		l, err := k.ClientSet.NetworkingV1().NetworkPolicies(ns).List(metav1.ListOptions{})
		if err != nil {
			return errors.Wrapf(err, "unable to list network policies in ns %s: %s", ns)
		}
		for _, np := range l.Items {
			log.Infof("deleting network policy %s in ns %s", np.Name, ns)
			err = k.ClientSet.NetworkingV1().NetworkPolicies(np.Namespace).Delete(np.Name, nil)
			if err != nil {
				return errors.Wrapf(err, "unable to delete network policy %s: %s", np.Name)
			}
		}
	}
	return nil
}

// CreateOrUpdateNetworkPolicy is a convenience function for upsdating/creating netpols.  Updating is important since
// some tests update a network policy to confirm that mutation works with a CNI.
func (k *Kubernetes) CreateOrUpdateNetworkPolicy(ns string, netpol *v1net.NetworkPolicy) (*v1net.NetworkPolicy, error) {
	log.Infof("creating/updating network policy %s in ns %s", netpol.Name, ns)
	netpol.ObjectMeta.Namespace = ns
	np, err := k.ClientSet.NetworkingV1().NetworkPolicies(ns).Update(netpol)
	if err == nil {
		return np, err
	}

	log.Debugf("unable to update network policy %s in ns %s, let's try creating it instead (error: %s)", netpol.Name, ns, err)
	np, err = k.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(netpol)
	if err != nil {
		log.Debugf("unable to create network policy: %s", err)
	}
	return np, err
}
