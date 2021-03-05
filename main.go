package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-resty/resty/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

//
// The annotations this controller uses for proxy discovery.
//
var externalEnodeURLAnnotation = "proxy.mantalabs.com/external-enode-url"
var internalEnodeURLAnnotation = "proxy.mantalabs.com/internal-enode-url"

var refreshPeriod = 10 * time.Second

type ErrorResponse struct {
	Jsonrpc string
	Id      int
	Error   struct {
		Code    int
		Message string
	}
}

type Proxy struct {
	ExternalEnodeUrl string
	InternalEnodeUrl string
}

type IstanbulGetProxies struct {
	Jsonrpc string
	Id      int
	Result  []Proxy
}

type IstanbulAddProxy struct {
	Jsonrpc string
	Id      int
	Result  bool
}

type IstanbulRemoveProxy struct {
	Jsonrpc string
	Id      int
	Result  bool
}

func main() {
	klog.InitFlags(nil)

	var kubeconfig string
	var rpcURL string
	var proxyNamespace string
	var proxyLabelSelector string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&rpcURL, "rpc-url", "http://127.0.0.1:8545", "RPC URL")
	flag.StringVar(&proxyNamespace, "proxy-namespace", "default", "namespace of proxy Pods")
	flag.StringVar(&proxyLabelSelector, "proxy-label-selector", "proxy=true", "label selector to select proxy Pods")
	flag.Parse()

	clientset, err := newClientset(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to connect to cluster: %v", err)
	}

	stopChan := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		klog.Info("Shutting down")
		close(stopChan)
	}()

	//
	// Watch events on Service objects matching a Kubernetes label selector.
	//
	// https://pkg.go.dev/k8s.io/client-go/tools/cache
	// https://pkg.go.dev/k8s.io/client-go/informers
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0,
		informers.WithNamespace(proxyNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = proxyLabelSelector
		}))
	informer := factory.Core().V1().Pods().Informer()

	validator, err := newValidator(rpcURL)
	if err != nil {
		klog.Fatalf("Failed to create Validator: %v", err)
	}

	controller, err := newController(validator, informer)
	if err != nil {
		klog.Fatalf("Failed to create Controller: %v", err)
	}

	controller.Run(stopChan)
}

type Validator struct {
	rpcURL string
}

func newValidator(rpcURL string) (*Validator, error) {
	validator := Validator{}
	validator.rpcURL = rpcURL
	return &validator, nil
}

func (validator *Validator) GetConfiguredProxies() ([]Proxy, error) {
	response, err := validator.rpc("istanbul_getProxiesInfo", nil, IstanbulGetProxies{})
	if err != nil {
		return nil, err
	}
	return response.(*IstanbulGetProxies).Result, nil
}

func (validator *Validator) rpc(method string, params []interface{}, result interface{}) (interface{}, error) {
	client := resty.New()

	resp, err := client.R().
		SetBody(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params, "id": 89999}).
		SetResult(result).
		Post(validator.rpcURL)
	if err != nil {
		klog.Warningf("HTTP %v failed: %v", method, err)
		return nil, err
	}

	var errorResponse ErrorResponse
	if err := json.Unmarshal(resp.Body(), &errorResponse); err != nil {
		klog.Warningf("failed to unmarshal response '%v': %v", resp, err)
		return nil, err
	}
	if errorResponse.Error.Code != 0 {
		return nil, fmt.Errorf("RPC error: %v", string(resp.Body()))
	}

	return resp.Result(), nil
}

type Controller struct {
	validator *Validator
	informer  cache.SharedIndexInformer
}

func newController(validator *Validator, podInformer cache.SharedIndexInformer) (*Controller, error) {
	controller := Controller{validator, podInformer}

	podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				klog.Infof("Added Pod %s, re-synchronizing", pod.ObjectMeta.Name)
				controller.Synchronize()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldPod := oldObj.(*corev1.Pod)
				klog.Infof("Updated Pod %s, re-synchronizing", oldPod.ObjectMeta.Name)
				controller.Synchronize()
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				klog.Infof("Deleted Pod %s, re-synchronizing", pod.ObjectMeta.Name)
				controller.Synchronize()
			},
		},
	)
	return &controller, nil
}

func (controller *Controller) Run(stopChan chan struct{}) {
	done := make(chan struct{})

	//
	// Expect the informer to keep the Validator proxy configuration synchronized
	// with Proxy Pods running in Kubernetes.
	//
	// Handle corner cases (e.g., Validator restarts) by periodically re-synchronizing.
	//
	go func() {
		controller.informer.Run(stopChan)
		klog.Info("Kubernetes informer done")
		done <- struct{}{}
	}()

	go func() {
		ticker := time.NewTicker(refreshPeriod)
		for {
			select {
			case <-ticker.C:
				controller.Synchronize()
			case <-stopChan:
				klog.Info("Controller done")
				done <- struct{}{}
				return
			}
		}
	}()

	<-done
	<-done
}

func (controller *Controller) GetCurrentProxies() ([]Proxy, error) {
	informer := controller.informer

	if !informer.HasSynced() {
		return nil, fmt.Errorf("Kubernetes state not synchronized")
	}

	pods := informer.GetStore().List()
	proxies := make(map[string]Proxy, 0)
	//
	// Look for Pods that have non-empty annotations for enode URLs and
	// group enode URLs by node ID to define Proxies. geth will resolve/lookup
	// hostnames into IPs before adding a Proxy to its configuration. Normalize
	// enode URLs with lookup so that all Proxy objects have IPs.
	//
	for _, pod := range pods {
		annotations := pod.(*corev1.Pod).ObjectMeta.GetAnnotations()

		externalEnodeURLString := annotations[externalEnodeURLAnnotation]
		if externalEnodeURLString != "" {
			externalEnodeURL, err := lookupURL(externalEnodeURLString)
			if err != nil && externalEnodeURL.User.Username() != "" {
				klog.Warningf("Failed to parse enode %v: %v", externalEnodeURLString, err)
			} else {
				proxy := proxies[externalEnodeURL.User.Username()]
				proxy.ExternalEnodeUrl = externalEnodeURL.String()
				proxies[externalEnodeURL.User.Username()] = proxy
			}
		}

		internalEnodeURLString := annotations[internalEnodeURLAnnotation]
		if internalEnodeURLString != "" {
			internalEnodeURL, err := lookupURL(internalEnodeURLString)
			if err != nil && internalEnodeURL.User.Username() != "" {
				klog.Warningf("Failed to parse enode %v: %v", internalEnodeURLString, err)
			} else {
				proxy := proxies[internalEnodeURL.User.Username()]
				proxy.InternalEnodeUrl = internalEnodeURL.String()
				proxies[internalEnodeURL.User.Username()] = proxy
			}
		}
	}

	result := make([]Proxy, 0)
	for _, proxy := range proxies {
		if proxy.InternalEnodeUrl != "" && proxy.ExternalEnodeUrl != "" {
			result = append(result, proxy)
		} else {
			klog.Infof("Skipping partially defined proxy: %+v", proxy)
		}
	}
	return result, nil
}

func (controller *Controller) Synchronize() error {
	//
	// Ensure the Validator proxy configuration matches the current Proxy Pods
	// we discover.
	// * Get the list of Proxies configured on the Validator
	// * Get the list of Proxies discovered on Kubernetes
	// * Calculate Proxies to remove from and to add to the Validator configuration
	// * Remove Proxies from Validator configuration
	// * Add Proxies to Valdiator configuration
	//

	configuredProxies, err := controller.validator.GetConfiguredProxies()
	if err != nil {
		return err
	}

	currentProxies, err := controller.GetCurrentProxies()
	if err != nil {
		return err
	}

	proxiesToRemove := make([]Proxy, 0)
	for _, configuredProxy := range configuredProxies {
		current := false
		for _, currentProxy := range currentProxies {
			if currentProxy == configuredProxy {
				current = true
				break
			}
		}
		// This proxy is configured with the Validator, but a Kubernetes Pod doesn't exist.
		if !current {
			proxiesToRemove = append(proxiesToRemove, configuredProxy)
		}
	}

	proxiesToAdd := make([]Proxy, 0)
	for _, currentProxy := range currentProxies {
		configured := false
		for _, configuredProxy := range configuredProxies {
			if currentProxy == configuredProxy {
				configured = true
				break
			}
		}
		// A Kubernetes Pod exists for this proxy, but it's not configured with the Validator.
		if !configured {
			proxiesToAdd = append(proxiesToAdd, currentProxy)
		}
	}

	//
	// Remove Proxies first. The Validator returns success but does not change
	// its configuration when we add a Proxy that shares a node ID with a
	// configured Proxy. We can avoid this by removing Proxies first.
	//
	for _, proxy := range proxiesToRemove {
		klog.Infof("Removing proxy: %+v\n", proxy)
		params := []interface{}{proxy.InternalEnodeUrl}
		_, err := controller.validator.rpc("istanbul_removeProxy", params, IstanbulRemoveProxy{})
		if err != nil {
			klog.Warningf("Failed to remove proxy from validator: %+v %v", proxy, err)
		}
	}

	for _, proxy := range proxiesToAdd {
		klog.Infof("Adding proxy: %+v\n", proxy)
		params := []interface{}{proxy.InternalEnodeUrl, proxy.ExternalEnodeUrl}
		_, err := controller.validator.rpc("istanbul_addProxy", params, IstanbulAddProxy{})
		if err != nil {
			klog.Warningf("Failed to add proxy to validator: %+v %v", proxy, err)
		}
	}

	return nil
}

func lookupURL(urlString string) (*url.URL, error) {
	result, err := url.Parse(urlString)
	if err != nil {
		return nil, err
	}

	hostname := result.Hostname()
	ip := net.ParseIP(hostname)
	// If ip == nil, assume hostname maps to a resolvable DNS entry.
	if ip == nil {
		ips, err := net.LookupIP(hostname)
		if err != nil {
			return nil, err
		}
		ip = ips[0]
		if result.Port() != "" {
			result.Host = ip.String() + ":" + result.Port()
		} else {
			result.Host = ip.String()
		}
	}
	return result, nil
}

func newClientset(filename string) (*kubernetes.Clientset, error) {
	config, err := getConfig(filename)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func getConfig(cfg string) (*rest.Config, error) {
	if cfg == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", cfg)
}
