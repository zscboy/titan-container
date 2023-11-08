package kube

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh/terminal"
	"io"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"os"

	"github.com/Filecoin-Titan/titan-container/node/config"
	"github.com/Filecoin-Titan/titan-container/node/impl/provider/kube/builder"
	logging "github.com/ipfs/go-log/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

type Client interface {
	Deploy(ctx context.Context, deployment builder.IClusterDeployment) error
	GetNS(ctx context.Context, ns string) (*v1.Namespace, error)
	DeleteNS(ctx context.Context, ns string) error
	FetchNodeResources(ctx context.Context) (map[string]*nodeResource, error)
	ListDeployments(ctx context.Context, ns string) (*appsv1.DeploymentList, error)
	ListStatefulSets(ctx context.Context, ns string) (*appsv1.StatefulSetList, error)
	ListServices(ctx context.Context, ns string) (*corev1.ServiceList, error)
	ListPods(ctx context.Context, ns string, opts metav1.ListOptions) (*corev1.PodList, error)
	PodLogs(ctx context.Context, ns string, podName string) (io.ReadCloser, error)
	Events(ctx context.Context, ns string, opts metav1.ListOptions) (*corev1.EventList, error)
	GetIngress(ctx context.Context, ns string, hostname string) (*netv1.Ingress, error)
	UpdateIngress(ctx context.Context, ns string, ingress *netv1.Ingress) (*netv1.Ingress, error)
	DeploymentCmdExec(ctx context.Context, cfgPath, ns, name string, stdin io.Reader, stdout, stderr io.Writer, tty bool,
		terminalSizeQueue remotecommand.TerminalSizeQueue) error
}

type client struct {
	kc             kubernetes.Interface
	metc           metricsclient.Interface
	log            *logging.ZapEventLogger
	providerConfig *config.ProviderCfg
}

func openKubeConfig(cfgPath string) (*rest.Config, error) {
	// Always bypass the default rate limiting
	rateLimiter := flowcontrol.NewFakeAlwaysRateLimiter()

	if cfgPath != "" {
		cfgPath = os.ExpandEnv(cfgPath)

		if _, err := os.Stat(cfgPath); err == nil {
			cfg, err := clientcmd.BuildConfigFromFlags("", cfgPath)
			if err != nil {
				return cfg, fmt.Errorf("%w: error building kubernetes config", err)
			}
			cfg.RateLimiter = rateLimiter
			return cfg, err
		}
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return cfg, fmt.Errorf("%w: error building kubernetes config", err)
	}
	cfg.RateLimiter = rateLimiter

	return cfg, err
}

func NewClient(kubeConfigPath string, providerConfig *config.ProviderCfg) (Client, error) {
	config, err := openKubeConfig(kubeConfigPath)
	if err != nil {
		return nil, err
	}

	// create the clientSet
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	metc, err := metricsclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	var log = logging.Logger("client")

	return &client{kc: clientSet, metc: metc, log: log, providerConfig: providerConfig}, nil
}

func (c *client) Deploy(ctx context.Context, deployment builder.IClusterDeployment) error {
	group := deployment.ManifestGroup()

	settingsI := ctx.Value(builder.SettingsKey)
	if nil == settingsI {
		return fmt.Errorf("kube client: not configured with settings in the context passed to function")
	}
	settings := settingsI.(builder.Settings)
	if err := builder.ValidateSettings(settings); err != nil {
		return err
	}

	ns := builder.BuildNS(settings, deployment)
	if err := applyNS(ctx, c.kc, builder.BuildNS(settings, deployment)); err != nil {
		c.log.Errorf("applying namespace %s err %s", ns.Name(), err.Error())
		return err
	}

	if err := applyNetPolicies(ctx, c.kc, builder.BuildNetPol(settings, deployment)); err != nil { //
		c.log.Errorf("applying namespace %s network policies err %s", ns.Name(), err)
		return err
	}

	for svcIdx := range group.Services {
		workload := builder.NewWorkload(settings, deployment, svcIdx)

		service := &group.Services[svcIdx]

		persistent := false
		for i := range service.Resources.Storage {
			attrVal := service.Resources.Storage[i].Attributes.Find(builder.StorageAttributePersistent)
			if persistent, _ = attrVal.AsBool(); persistent {
				break
			}
		}

		if persistent {
			if err := applyStatefulSet(ctx, c.kc, builder.BuildStatefulSet(workload)); err != nil {
				c.log.Errorf("applying statefulSet err %s, ns %s, service %s", err.Error(), ns.Name(), service.Name)
				return err
			}
		} else {
			if err := applyDeployment(ctx, c.kc, builder.NewDeployment(workload)); err != nil {
				c.log.Errorf("applying deployment err %s, ns %s, service %s", err.Error(), ns.Name(), service.Name)
				return err
			}
		}

		if len(service.Expose) == 0 {
			c.log.Debug("no services", "ns", ns.Name(), "service", service.Name)
			continue
		}

		serviceBuilderLocal := builder.BuildService(workload, false)
		if serviceBuilderLocal.Any() {
			if err := applyService(ctx, c.kc, serviceBuilderLocal); err != nil {
				c.log.Errorf("applying local service err %s, ns %s, service %s", err.Error(), ns.Name(), service.Name)
				return err
			}
		}

		serviceBuilderGlobal := builder.BuildService(workload, true)
		if serviceBuilderGlobal.Any() {
			if err := applyService(ctx, c.kc, serviceBuilderGlobal); err != nil {
				c.log.Errorf("applying global service err %s, ns %s, service %s", err.Error(), ns.Name(), service.Name)
				return err
			}
		}

		if len(c.providerConfig.HostName) > 0 {
			for _, expose := range service.Expose {
				if builder.ShouldBeIngress(expose) {
					hostDirective := builder.BuildHostNameDirective(ns.Name(), c.providerConfig.HostName, service.Name, c.providerConfig.IngressClassName, expose, c.providerConfig.UseCaddyIngress)
					if err := applyIngress(ctx, c.kc, builder.BuildIngress(workload, hostDirective)); err != nil {
						c.log.Errorf("applying ingress error %s, ns %s, service %s", err.Error(), ns.Name(), service.Name)
						return err
					}
				}
			}
		}

	}

	return nil
}

func (c *client) DeleteNS(ctx context.Context, ns string) error {
	return c.kc.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
}

func (c *client) GetNS(ctx context.Context, ns string) (*v1.Namespace, error) {
	return c.kc.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
}

func (c *client) ListServices(ctx context.Context, ns string) (*corev1.ServiceList, error) {
	return c.kc.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
}

func (c *client) ListDeployments(ctx context.Context, ns string) (*appsv1.DeploymentList, error) {
	return c.kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
}

func (c *client) ListStatefulSets(ctx context.Context, ns string) (*appsv1.StatefulSetList, error) {
	return c.kc.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
}

func (c *client) ListPods(ctx context.Context, ns string, opts metav1.ListOptions) (*corev1.PodList, error) {
	return c.kc.CoreV1().Pods(ns).List(ctx, opts)
}

func (c *client) PodLogs(ctx context.Context, ns string, podName string) (io.ReadCloser, error) {
	return c.kc.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{}).Stream(context.Background())
}

func (c *client) Events(ctx context.Context, ns string, opts metav1.ListOptions) (*corev1.EventList, error) {
	return c.kc.CoreV1().Events(ns).List(ctx, opts)
}

func (c *client) GetIngress(ctx context.Context, ns string, hostname string) (*netv1.Ingress, error) {
	ingressName := builder.NewHostName(ns, hostname)
	return c.kc.NetworkingV1().Ingresses(ns).Get(ctx, ingressName, metav1.GetOptions{})
}

func (c *client) UpdateIngress(ctx context.Context, ns string, ingress *netv1.Ingress) (*netv1.Ingress, error) {
	return c.kc.NetworkingV1().Ingresses(ns).Update(ctx, ingress, metav1.UpdateOptions{})
}

func (c *client) DeploymentCmdExec(ctx context.Context, cfgPath, ns, name string, stdin io.Reader, stdout, stderr io.Writer, tty bool,
	terminalSizeQueue remotecommand.TerminalSizeQueue) error {
	pod, err := c.kc.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	restCfg, err := openKubeConfig(cfgPath)
	if err != nil {
		return err
	}

	request := c.kc.CoreV1().RESTClient().Post().Resource("pods").Name(name).Namespace(ns).SubResource("exec").VersionedParams(
		&corev1.PodExecOptions{
			Command: []string{"sh"},
			Stdin:   stdin != nil,
			Stdout:  stdout != nil,
			Stderr:  stderr != nil,
			TTY:     tty,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", request.URL())

	if !terminal.IsTerminal(0) || !terminal.IsTerminal(1) {
		return err
	}

	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		return err
	}
	defer terminal.Restore(0, oldState)

	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		Tty:               tty,
		TerminalSizeQueue: terminalSizeQueue,
	})
	if err != nil {
		return errors.Errorf("Failed executing: %v %s on %s", err, pod.Namespace, pod.Name)
	}

	return nil
}
