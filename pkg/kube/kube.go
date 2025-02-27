package kube

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	// load auth plugins
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/exec"

	// only required to authenticate against GKE clusters
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	_ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/werf/kubedog/pkg/utils"
)

const (
	kubeTokenFilePath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	kubeNamespaceFilePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

var (
	Kubernetes       kubernetes.Interface
	Client           kubernetes.Interface
	DynamicClient    dynamic.Interface
	DefaultNamespace string
	Context          string
)

type InitOptions struct {
	KubeConfigOptions
}

func Init(opts InitOptions) error {
	config, err := GetKubeConfig(opts.KubeConfigOptions)
	if err != nil {
		return err
	}

	if config != nil {
		clientset, err := kubernetes.NewForConfig(config.Config)
		if err != nil {
			return err
		}
		Kubernetes = clientset
		Client = clientset

		dynamicClient, err := dynamic.NewForConfig(config.Config)
		if err != nil {
			return err
		}
		DynamicClient = dynamicClient
	}

	return nil
}

type KubeConfigOptions struct {
	Context             string
	ConfigPath          string
	ConfigDataBase64    string
	ConfigPathMergeList []string
}

type KubeConfig struct {
	Config           *rest.Config
	Context          string
	DefaultNamespace string
}

func GetKubeConfig(opts KubeConfigOptions) (*KubeConfig, error) {
	// Try to load from kubeconfig in flags or from ~/.kube/config
	config, outOfClusterErr := getOutOfClusterConfig(opts.Context, opts.ConfigPath, opts.ConfigDataBase64, opts.ConfigPathMergeList)

	if config == nil {
		if hasInClusterConfig() {
			// Try to configure as inCluster
			if config, err := getInClusterConfig(); err != nil {
				if opts.ConfigPath != "" || opts.Context != "" || opts.ConfigDataBase64 != "" {
					if outOfClusterErr != nil {
						return nil, fmt.Errorf("out-of-cluster config error: %v, in-cluster config error: %v", outOfClusterErr, err)
					}
				} else {
					return nil, err
				}
			} else if config != nil {
				return config, nil
			}
		} else {
			// if not in cluster return outOfCluster error
			if outOfClusterErr != nil {
				return nil, outOfClusterErr
			}
		}

		return nil, nil
	}

	return config, outOfClusterErr
}

type GetAllContextsClientsOptions struct {
	ConfigPath          string
	ConfigDataBase64    string
	ConfigPathMergeList []string
}

type ContextClient struct {
	ContextName      string
	ContextNamespace string
	Client           kubernetes.Interface
}

func GetAllContextsClients(opts GetAllContextsClientsOptions) ([]*ContextClient, error) {
	// Try to load contexts from kubeconfig in flags or from ~/.kube/config
	var outOfClusterErr error
	contexts, outOfClusterErr := getOutOfClusterContextsClients(opts.ConfigPath, opts.ConfigDataBase64, opts.ConfigPathMergeList)
	// return if contexts are loaded successfully
	if contexts != nil {
		return contexts, nil
	}
	if hasInClusterConfig() {
		contextClient, err := getInClusterContextClient()
		if err != nil {
			return nil, err
		}

		return []*ContextClient{contextClient}, nil
	}
	// if not in cluster return outOfCluster error
	if outOfClusterErr != nil {
		return nil, outOfClusterErr
	}

	return nil, nil
}

func makeOutOfClusterClientConfigError(configPath, context string, err error) error {
	baseErrMsg := "out-of-cluster configuration problem"

	if configPath != "" {
		baseErrMsg += fmt.Sprintf(", custom kube config path is %q", configPath)
	}

	if context != "" {
		baseErrMsg += fmt.Sprintf(", custom kube context is %q", context)
	}

	return fmt.Errorf("%s: %s", baseErrMsg, err)
}

func GetClientConfig(context string, configPath string, configData []byte, configPathMergeList []string) (clientcmd.ClientConfig, error) {
	overrides := &clientcmd.ConfigOverrides{ClusterDefaults: clientcmd.ClusterDefaults}
	if context != "" {
		overrides.CurrentContext = context
	}

	if configData != nil {
		config, err := clientcmd.Load(configData)
		if err != nil {
			return nil, fmt.Errorf("unable to load config data: %s", err)
		}

		return clientcmd.NewDefaultClientConfig(*config, overrides), nil
	}

	if len(configPathMergeList) > 0 {
		oldEnvVar := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)
		defer func() {
			os.Setenv(clientcmd.RecommendedConfigPathEnvVar, oldEnvVar)
		}()

		configPathEnvVar := strings.Join(configPathMergeList, string(filepath.ListSeparator))
		if err := os.Setenv(clientcmd.RecommendedConfigPathEnvVar, configPathEnvVar); err != nil {
			return nil, fmt.Errorf("unable to set env var %q: %s", clientcmd.RecommendedConfigPathEnvVar, err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	if configPath != "" {
		rules.ExplicitPath = configPath
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides), nil
}

func hasInClusterConfig() bool {
	token, _ := utils.FileExists(kubeTokenFilePath)
	ns, _ := utils.FileExists(kubeNamespaceFilePath)
	return token && ns
}

func parseConfigDataBase64(configDataBase64 string) ([]byte, error) {
	var configData []byte

	if configDataBase64 != "" {
		if data, err := base64.StdEncoding.DecodeString(configDataBase64); err != nil {
			return nil, fmt.Errorf("unable to decode base64 config data: %s", err)
		} else {
			configData = data
		}
	}

	return configData, nil
}

func getOutOfClusterConfig(context, configPath, configDataBase64 string, configPathMergeList []string) (*KubeConfig, error) {
	res := &KubeConfig{}

	configData, err := parseConfigDataBase64(configDataBase64)
	if err != nil {
		return nil, fmt.Errorf("unable to parse base64 config data: %s", err)
	}

	clientConfig, err := GetClientConfig(context, configPath, configData, configPathMergeList)
	if err != nil {
		return nil, makeOutOfClusterClientConfigError(configPath, context, err)
	}

	if ns, _, err := clientConfig.Namespace(); err != nil {
		return nil, fmt.Errorf("cannot determine default kubernetes namespace: %s", err)
	} else {
		res.DefaultNamespace = ns
	}

	config, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, makeOutOfClusterClientConfigError(configPath, context, err)
	}
	if config == nil {
		return nil, nil
	}
	res.Config = config

	if context == "" {
		if rc, err := clientConfig.RawConfig(); err != nil {
			return nil, fmt.Errorf("cannot get raw kubernetes config: %s", err)
		} else {
			res.Context = rc.CurrentContext
		}
	} else {
		res.Context = context
	}

	return res, nil
}

func getOutOfClusterContextsClients(configPath, configDataBase64 string, configPathMergeList []string) ([]*ContextClient, error) {
	res := make([]*ContextClient, 0)

	configData, err := parseConfigDataBase64(configDataBase64)
	if err != nil {
		return nil, fmt.Errorf("unable to parse base64 config data: %s", err)
	}

	clientConfig, err := GetClientConfig("", configPath, configData, configPathMergeList)
	if err != nil {
		return nil, err
	}

	rc, err := clientConfig.RawConfig()
	if err != nil {
		return nil, err
	}

	for contextName, context := range rc.Contexts {
		clientConfig, err := GetClientConfig(contextName, configPath, configData, configPathMergeList)
		if err != nil {
			return nil, makeOutOfClusterClientConfigError(configPath, contextName, err)
		}

		config, err := clientConfig.ClientConfig()
		if err != nil {
			return nil, makeOutOfClusterClientConfigError(configPath, contextName, err)
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, err
		}

		res = append(res, &ContextClient{
			ContextName:      contextName,
			ContextNamespace: context.Namespace,
			Client:           clientset,
		})
	}

	return res, nil
}

func getInClusterConfig() (*KubeConfig, error) {
	res := &KubeConfig{}

	if config, err := rest.InClusterConfig(); err != nil {
		return nil, fmt.Errorf("in-cluster configuration problem: %s", err)
	} else {
		res.Config = config
	}

	if data, err := ioutil.ReadFile(kubeNamespaceFilePath); err != nil {
		return nil, fmt.Errorf("in-cluster configuration problem: cannot determine default kubernetes namespace: error reading %s: %s", kubeNamespaceFilePath, err)
	} else {
		res.DefaultNamespace = string(data)
	}

	return res, nil
}

func getInClusterContextClient() (*ContextClient, error) {
	kubeConfig, err := getInClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig.Config)
	if err != nil {
		return nil, err
	}

	return &ContextClient{
		ContextName:      "inClusterContext",
		ContextNamespace: kubeConfig.DefaultNamespace,
		Client:           clientset,
	}, nil
}

func GroupVersionResourceByKind(client kubernetes.Interface, kind string) (schema.GroupVersionResource, error) {
	lists, err := client.Discovery().ServerPreferredResources()
	if err != nil {
		return schema.GroupVersionResource{}, err
	}

	for _, list := range lists {
		if len(list.APIResources) == 0 {
			continue
		}

		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}

		for _, resource := range list.APIResources {
			if len(resource.Verbs) == 0 {
				continue
			}

			if kind == resource.Kind {
				groupVersionResource := schema.GroupVersionResource{
					Resource: resource.Name,
					Group:    gv.Group,
					Version:  gv.Version,
				}

				return groupVersionResource, nil
			}
		}
	}

	return schema.GroupVersionResource{}, fmt.Errorf("kind %s is not supported", kind)
}
