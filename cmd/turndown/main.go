package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	clientset "github.com/kubecost/cluster-turndown/v2/pkg/generated/clientset/versioned"
	informers "github.com/kubecost/cluster-turndown/v2/pkg/generated/informers/externalversions"

	cp "github.com/kubecost/cluster-turndown/v2/pkg/cluster/provider"

	"github.com/kubecost/cluster-turndown/v2/pkg/signals"
	"github.com/kubecost/cluster-turndown/v2/pkg/turndown"
	"github.com/kubecost/cluster-turndown/v2/pkg/turndown/provider"
	"github.com/kubecost/cluster-turndown/v2/pkg/turndown/strategy"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Run web server with turndown endpoints
func runWebServer(kubeClient kubernetes.Interface, client clientset.Interface, scheduler *turndown.TurndownScheduler, manager turndown.TurndownManager, provider provider.TurndownProvider) {
	mux := http.NewServeMux()

	endpoints := turndown.NewTurndownEndpoints(kubeClient, client, scheduler, manager, provider)

	mux.HandleFunc("/schedule", endpoints.HandleStartSchedule)
	mux.HandleFunc("/cancel", endpoints.HandleCancelSchedule)

	log.Fatal().Msgf("%s", http.ListenAndServe(":9731", mux))
}

// Initialize Kubernetes Client as well as the CRD Client
func initKubernetes(isLocal bool) (kubernetes.Interface, clientset.Interface, error) {
	var kc *rest.Config
	var err error

	// For local testing, use kubeconfig in home directory
	if isLocal {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}

		configFile := filepath.Join(homeDir, ".kube", "config")
		log.Info().Msgf("KubeConfig Path: %s", configFile)

		kc, err = clientcmd.BuildConfigFromFlags("", configFile)
		if err != nil {
			return nil, nil, err
		}
	} else {
		kc, err = rest.InClusterConfig()
		if err != nil {
			return nil, nil, err
		}
	}

	kubeClient, err := kubernetes.NewForConfig(kc)
	if err != nil {
		return nil, nil, err
	}

	client, err := clientset.NewForConfig(kc)
	if err != nil {
		return nil, nil, err
	}

	return kubeClient, client, nil
}

// Runs a controller loop to ensure that our custom resource definition: TurndownSchedule is handled properly
// by the API.
func runTurndownResourceController(kubeClient kubernetes.Interface, tdClient clientset.Interface, scheduler *turndown.TurndownScheduler, stopCh <-chan struct{}) {
	tdInformer := informers.NewSharedInformerFactory(tdClient, time.Second*30)
	controller := turndown.NewTurndownScheduleResourceController(kubeClient, tdClient, scheduler, tdInformer.Kubecost().V1alpha1().TurndownSchedules())
	tdInformer.Start(stopCh)

	go func(c *turndown.TurndownScheduleResourceController, s <-chan struct{}) {
		if err := c.Run(1, s); err != nil {
			log.Fatal().Msgf("Error running controller: %s", err.Error())
		}
	}(controller, stopCh)
}

// For now, we'll choose our strategy based on the provider, but functionally, there is
// no dependency.
func strategyForProvider(c kubernetes.Interface, p provider.TurndownProvider) (strategy.TurndownStrategy, error) {
	m := make(map[string]string)

	switch v := p.(type) {
	case *provider.GKEProvider:
		return strategy.NewMasterlessTurndownStrategy(c, p, m), nil
	case *provider.EKSProvider:
		return strategy.NewMasterlessTurndownStrategy(c, p, m), nil
	case *provider.AWSProvider:
		return strategy.NewStandardTurndownStrategy(c, p), nil
	default:
		return nil, fmt.Errorf("No strategy available for: %+v", v)
	}
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	// TODO: Make configurable
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	stopCh := signals.SetupSignalHandler()

	node := os.Getenv("NODE_NAME")
	log.Info().Msgf("Running Kubecost Turndown on: %s", node)

	// Setup Components
	kubeClient, tdClient, err := initKubernetes(false)
	if err != nil {
		log.Fatal().Msgf("Failed to initialize kubernetes client: %s", err.Error())
	}

	// Schedule Persistence via Kubernetes Custom Resource Definition
	scheduleStore := turndown.NewKubernetesScheduleStore(tdClient)
	//scheduleStore := turndown.NewDiskScheduleStore("/var/configs/schedule.json")

	// Platform Provider API
	clusterProvider, err := cp.NewClusterProvider(kubeClient)
	if err != nil {
		log.Error().Msgf("Failed to create ClusterProvider: %s", err.Error())
		return
	}

	// Turndown Provider API
	turndownProvider, err := provider.NewTurndownProvider(kubeClient, clusterProvider)
	if err != nil {
		log.Error().Msgf("Failed to determine provider: %s", err.Error())
		return
	}

	// Validate TurndownProvider
	err = provider.Validate(turndownProvider, 5)
	if err != nil {
		log.Error().Msgf("[Error]: Failed to validate provider: %s", err.Error())
		return
	}

	// Determine the best turndown strategy to use based on provider
	strategy, err := strategyForProvider(kubeClient, turndownProvider)
	if err != nil {
		log.Error().Msgf("Failed to create strategy: %s", err.Error())
		return
	}

	// Turndown Management and Scheduler
	manager := turndown.NewKubernetesTurndownManager(kubeClient, turndownProvider, strategy, node)
	scheduler := turndown.NewTurndownScheduler(manager, scheduleStore)

	// Run TurndownSchedule Kubernetes Resource Controller
	runTurndownResourceController(kubeClient, tdClient, scheduler, stopCh)

	// Run Turndown Endpoints
	runWebServer(kubeClient, tdClient, scheduler, manager, turndownProvider)
}
