package apply

import (
	"encoding/json"
	"fmt"
	"github.com/alibaba/open-simulator/pkg/algo"
	"github.com/alibaba/open-simulator/pkg/chart"
	"github.com/alibaba/open-simulator/pkg/simulator"
	simontype "github.com/alibaba/open-simulator/pkg/type"
	"github.com/alibaba/open-simulator/pkg/utils"
	simonv1 "github.com/alibaba/open-simulator/pkg/v1"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	configv1alpha1 "k8s.io/component-base/config/v1alpha1"
	"k8s.io/component-base/logs"
	kubeschedulerconfigv1beta1 "k8s.io/kube-scheduler/config/v1beta1"
	"k8s.io/kubernetes/cmd/kube-scheduler/app/config"
	schedoptions "k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	kubeschedulerscheme "k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"os"
	"sigs.k8s.io/yaml"
	"sort"
)

type Options struct {
	SimonConfig                string
	DefaultSchedulerConfigFile string
	UseGreed                   bool
	Interactive                bool
}

type DefaulterApply struct {
	Cluster         simonv1.Cluster
	AppList         []simonv1.AppInfo
	NewNode         string
	SchedulerConfig string
	UseGreed        bool
}

func (applier *DefaulterApply) Run(opts Options) (err error) {
	var resourceList []simontype.ResourceInfo

	// Step 0: check args of Options
	if err = applier.ParseArgsAndConfigFile(opts); err != nil {
		return fmt.Errorf("Parse Error: %v ", err)
	}

	if err = applier.Validate(); err != nil {
		return fmt.Errorf("Invalid information: %v ", err)
	}

	for _, app := range applier.AppList {
		newPath := app.Path

		if app.Chart {
			outputDir, err := chart.ProcessChart(app.Name, app.Path)
			if err != nil {
				return err
			}
			newPath = outputDir
		}

		// Step 1: convert recursively the application directory into a series of file paths
		appFilePaths, err := utils.ParseFilePath(newPath)
		if err != nil {
			return fmt.Errorf("Failed to parse the application config path: %v ", err)
		}

		// Step 2: convert yml or yaml file of the application files to kubernetes appResources
		appResource, err := utils.GetObjectsFromFiles(appFilePaths)
		if err != nil {
			return fmt.Errorf("%v", err)
		}

		newResource := simontype.ResourceInfo{
			Name:     app.Name,
			Resource: appResource,
		}
		resourceList = append(resourceList, newResource)
	}

	newNode, exist := utils.DecodeYamlFile(applier.NewNode).(*corev1.Node)
	if !exist {
		return fmt.Errorf("The NewNode file(%s) is not a Node yaml ", applier.NewNode)
	}

	// Step 3: generate kube-client
	kubeClient, err := applier.generateKubeClient()
	if err != nil {
		return fmt.Errorf("Failed to get kubeclient: %v ", err)
	}

	// Step 4: get scheduler CompletedConfig and set the list of scheduler bind plugins to Simon.
	cc, err := applier.getAndSetSchedulerConfig()
	if err != nil {
		return err
	}

	// Step 5: get result
	for i := 0; i < 100; i++ {
		// init simulator
		sim, err := simulator.New(kubeClient, cc)
		if err != nil {
			return err
		}

		// start a scheduler as a goroutine
		sim.RunScheduler()

		// synchronize resources from real or simulated cluster to fake cluster
		if err := sim.CreateFakeCluster(applier.Cluster.CustomCluster); err != nil {
			return fmt.Errorf("create fake cluster failed: %s", err.Error())
		}

		// add nodes to get a successful scheduling
		if err := sim.AddNewNode(newNode, i); err != nil {
			return err
		}

		// success: to determine whether the current resource is successfully scheduled
		// added: the daemon pods derived from the cluster daemonset only need to be added once
		success, added := false, false
		for _, resourceInfo := range resourceList {
			success = false
			// synchronize pods generated by deployment、daemonset and like this, then format all unscheduled pods
			appPods := simulator.GenerateValidPodsFromResources(sim.GetFakeClient(), resourceInfo.Resource)
			if !added {
				appPods = append(appPods, sim.GenerateValidDaemonPodsForNewNode()...)
				added = true
			}

			// sort pods
			if applier.UseGreed {
				greed := algo.NewGreedQueue(sim.GetNodes(), appPods)
				sort.Sort(greed)
				// tol := algo.NewTolerationQueue(pods)
				// sort.Sort(tol)
				// aff := algo.NewAffinityQueue(pods)
				// sort.Sort(aff)
			}

			fmt.Printf(utils.ColorCyan+"%s: %d pods to be simulated, %d pods of which to be scheduled\n"+utils.ColorReset, resourceInfo.Name, len(appPods), utils.GetTotalNumberOfPodsWithoutNodeName(appPods))
			err = sim.SchedulePods(appPods)
			if err != nil {
				fmt.Printf(utils.ColorRed+"%s: %s\n"+utils.ColorReset, resourceInfo.Name, err.Error())
				break
			} else {
				success = true
				fmt.Printf(utils.ColorGreen+"%s: Success!", resourceInfo.Name)
				sim.Report()
				fmt.Println(utils.ColorReset)
				if err := sim.CreateConfigMapAndSaveItToFile(simontype.ConfigMapFileName); err != nil {
					return err
				}
				if opts.Interactive {
					prompt := fmt.Sprintf("%s scheduled succeessfully, continue(y/n)?", resourceInfo.Name)
					if utils.Confirm(prompt) {
						continue
					} else {
						break
					}
				}
			}
		}
		sim.Close()

		if success {
			fmt.Printf(utils.ColorCyan + "Congratulations! A Successful Scheduling!" + utils.ColorReset)
			break
		}
	}
	return nil
}

// generateKubeClient generates kube-client by kube-config. And if kube-config file is not provided, the value of kube-client will be nil
func (applier *DefaulterApply) generateKubeClient() (*clientset.Clientset, error) {
	if len(applier.Cluster.KubeConfig) == 0 {
		return nil, nil
	}

	var err error
	var cfg *restclient.Config
	master, err := utils.GetMasterFromKubeConfig(applier.Cluster.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse kubeclient file: %v ", err)
	}

	cfg, err = clientcmd.BuildConfigFromFlags(master, applier.Cluster.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("Unable to build config: %v ", err)
	}

	kubeClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return kubeClient, nil
}

// getAndSetSchedulerConfig gets scheduler CompletedConfig and sets the list of scheduler bind plugins to Simon.
func (applier *DefaulterApply) getAndSetSchedulerConfig() (*config.CompletedConfig, error) {
	versionedCfg := kubeschedulerconfigv1beta1.KubeSchedulerConfiguration{}
	versionedCfg.DebuggingConfiguration = *configv1alpha1.NewRecommendedDebuggingConfiguration()
	kubeschedulerscheme.Scheme.Default(&versionedCfg)
	kcfg := kubeschedulerconfig.KubeSchedulerConfiguration{}
	if err := kubeschedulerscheme.Scheme.Convert(&versionedCfg, &kcfg, nil); err != nil {
		return nil, err
	}
	if len(kcfg.Profiles) == 0 {
		kcfg.Profiles = []kubeschedulerconfig.KubeSchedulerProfile{
			{},
		}
	}
	kcfg.Profiles[0].SchedulerName = corev1.DefaultSchedulerName
	if kcfg.Profiles[0].Plugins == nil {
		kcfg.Profiles[0].Plugins = &kubeschedulerconfig.Plugins{}
	}

	if applier.UseGreed {
		kcfg.Profiles[0].Plugins.Score = &kubeschedulerconfig.PluginSet{
			Enabled: []kubeschedulerconfig.Plugin{{Name: simontype.SimonPluginName}},
		}
	}
	kcfg.Profiles[0].Plugins.Bind = &kubeschedulerconfig.PluginSet{
		Enabled:  []kubeschedulerconfig.Plugin{{Name: simontype.SimonPluginName}},
		Disabled: []kubeschedulerconfig.Plugin{{Name: defaultbinder.Name}},
	}
	// set percentageOfNodesToScore value to 100
	kcfg.PercentageOfNodesToScore = 100
	opts := &schedoptions.Options{
		ComponentConfig: kcfg,
		ConfigFile:      applier.SchedulerConfig,
		Logs:            logs.NewOptions(),
	}
	cc, err := utils.InitKubeSchedulerConfiguration(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to init kube scheduler configuration: %v ", err)
	}
	return cc, nil
}

func (applier *DefaulterApply) ParseArgsAndConfigFile(opts Options) error {
	simonCR := &simonv1.Simon{}
	configFile, err := ioutil.ReadFile(opts.SimonConfig)
	if err != nil {
		return fmt.Errorf("failed to read config file(%s): %v", opts.SimonConfig, err)
	}
	configJSON, err := yaml.YAMLToJSON(configFile)
	if err != nil {
		return fmt.Errorf("failed to unmarshal config file(%s) to json: %v", opts.SimonConfig, err)
	}

	if err := json.Unmarshal(configJSON, simonCR); err != nil {
		return fmt.Errorf("failed to unmarshal config json to object: %v", err)
	}

	applier.Cluster = simonCR.Spec.Cluster
	applier.AppList = simonCR.Spec.AppList
	applier.NewNode = simonCR.Spec.NewNode
	applier.SchedulerConfig = opts.DefaultSchedulerConfigFile
	applier.UseGreed = opts.UseGreed

	return nil
}

func (applier *DefaulterApply) Validate() error {
	if len(applier.Cluster.KubeConfig) == 0 && len(applier.Cluster.CustomCluster) == 0 ||
		len(applier.Cluster.KubeConfig) != 0 && len(applier.Cluster.CustomCluster) != 0 {
		return fmt.Errorf("only one of values of both kubeConfig and customConfig must exist")
	}

	if len(applier.Cluster.KubeConfig) != 0 {
		if _, err := os.Stat(applier.Cluster.KubeConfig); err != nil {
			return fmt.Errorf("invalid path of kubeConfig: %v", err)
		}
	}

	if len(applier.Cluster.CustomCluster) != 0 {
		if _, err := os.Stat(applier.Cluster.CustomCluster); err != nil {
			return fmt.Errorf("invalid path of customConfig: %v", err)
		}
	}

	if len(applier.SchedulerConfig) != 0 {
		if _, err := os.Stat(applier.SchedulerConfig); err != nil {
			return fmt.Errorf("invalid path of scheduler config: %v", err)
		}
	}

	if len(applier.NewNode) != 0 {
		if _, err := os.Stat(applier.NewNode); err != nil {
			return fmt.Errorf("invalid path of newNode: %v", err)
		}
	}

	for _, app := range applier.AppList {
		if _, err := os.Stat(app.Path); err != nil {
			return fmt.Errorf("invalid path of %s app: %v", app.Name, err)
		}
	}

	return nil
}
