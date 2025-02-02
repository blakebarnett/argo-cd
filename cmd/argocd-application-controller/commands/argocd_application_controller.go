package commands

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/argoproj/pkg/stats"
	"github.com/go-redis/redis/v8"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	cmdutil "github.com/argoproj/argo-cd/v2/cmd/util"
	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/controller"
	"github.com/argoproj/argo-cd/v2/controller/sharding"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v2/reposerver/apiclient"
	cacheutil "github.com/argoproj/argo-cd/v2/util/cache"
	appstatecache "github.com/argoproj/argo-cd/v2/util/cache/appstate"
	"github.com/argoproj/argo-cd/v2/util/cli"
	"github.com/argoproj/argo-cd/v2/util/env"
	"github.com/argoproj/argo-cd/v2/util/errors"
	kubeutil "github.com/argoproj/argo-cd/v2/util/kube"
	"github.com/argoproj/argo-cd/v2/util/settings"
	"github.com/argoproj/argo-cd/v2/util/tls"
)

const (
	// CLIName is the name of the CLI
	cliName = "argocd-application-controller"
	// Default time in seconds for application resync period
	defaultAppResyncPeriod = 180
)

func NewCommand() *cobra.Command {
	var (
		clientConfig             clientcmd.ClientConfig
		appResyncPeriod          int64
		repoServerAddress        string
		repoServerTimeoutSeconds int
		selfHealTimeoutSeconds   int
		statusProcessors         int
		operationProcessors      int
		glogLevel                int
		metricsPort              int
		metricsCacheExpiration   time.Duration
		kubectlParallelismLimit  int64
		cacheSrc                 func() (*appstatecache.Cache, error)
		redisClient              *redis.Client
		repoServerPlaintext      bool
		repoServerStrictTLS      bool
	)
	var command = cobra.Command{
		Use:               cliName,
		Short:             "Run ArgoCD Application Controller",
		Long:              "ArgoCD application controller is a Kubernetes controller that continuously monitors running applications and compares the current, live state against the desired target state (as specified in the repo). This command runs Application Controller in the foreground.  It can be configured by following options.",
		DisableAutoGenTag: true,
		RunE: func(c *cobra.Command, args []string) error {
			cli.SetLogFormat(cmdutil.LogFormat)
			cli.SetLogLevel(cmdutil.LogLevel)
			cli.SetGLogLevel(glogLevel)

			config, err := clientConfig.ClientConfig()
			errors.CheckError(err)
			errors.CheckError(v1alpha1.SetK8SConfigDefaults(config))

			kubeClient := kubernetes.NewForConfigOrDie(config)
			appClient := appclientset.NewForConfigOrDie(config)

			namespace, _, err := clientConfig.Namespace()
			errors.CheckError(err)

			resyncDuration := time.Duration(appResyncPeriod) * time.Second
			tlsConfig := apiclient.TLSConfiguration{
				DisableTLS:       repoServerPlaintext,
				StrictValidation: repoServerStrictTLS,
			}

			// Load CA information to use for validating connections to the
			// repository server, if strict TLS validation was requested.
			if !repoServerPlaintext && repoServerStrictTLS {
				pool, err := tls.LoadX509CertPool(
					fmt.Sprintf("%s/controller/tls/tls.crt", env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)),
					fmt.Sprintf("%s/controller/tls/ca.crt", env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)),
				)
				if err != nil {
					log.Fatalf("%v", err)
				}
				tlsConfig.Certificates = pool
			}

			repoClientset := apiclient.NewRepoServerClientset(repoServerAddress, repoServerTimeoutSeconds, tlsConfig)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cache, err := cacheSrc()
			errors.CheckError(err)
			cache.Cache.SetClient(cacheutil.NewTwoLevelClient(cache.Cache.GetClient(), 10*time.Minute))

			settingsMgr := settings.NewSettingsManager(ctx, kubeClient, namespace)
			kubectl := kubeutil.NewKubectl()
			clusterFilter := getClusterFilter()
			appController, err := controller.NewApplicationController(
				namespace,
				settingsMgr,
				kubeClient,
				appClient,
				repoClientset,
				cache,
				kubectl,
				resyncDuration,
				time.Duration(selfHealTimeoutSeconds)*time.Second,
				metricsPort,
				metricsCacheExpiration,
				kubectlParallelismLimit,
				clusterFilter)
			errors.CheckError(err)
			cacheutil.CollectMetrics(redisClient, appController.GetMetricsServer())

			vers := common.GetVersion()
			log.Infof("Application Controller (version: %s, built: %s) starting (namespace: %s)", vers.Version, vers.BuildDate, namespace)
			stats.RegisterStackDumper()
			stats.StartStatsTicker(10 * time.Minute)
			stats.RegisterHeapDumper("memprofile")

			go appController.Run(ctx, statusProcessors, operationProcessors)

			// Wait forever
			select {}
		},
	}

	clientConfig = cli.AddKubectlFlagsToCmd(&command)
	command.Flags().Int64Var(&appResyncPeriod, "app-resync", int64(env.ParseDurationFromEnv("ARGOCD_RECONCILIATION_TIMEOUT", defaultAppResyncPeriod, 0, math.MaxInt32).Seconds()), "Time period in seconds for application resync.")
	command.Flags().StringVar(&repoServerAddress, "repo-server", common.DefaultRepoServerAddr, "Repo server address.")
	command.Flags().IntVar(&repoServerTimeoutSeconds, "repo-server-timeout-seconds", 60, "Repo server RPC call timeout seconds.")
	command.Flags().IntVar(&statusProcessors, "status-processors", 1, "Number of application status processors")
	command.Flags().IntVar(&operationProcessors, "operation-processors", 1, "Number of application operation processors")
	command.Flags().StringVar(&cmdutil.LogFormat, "logformat", "text", "Set the logging format. One of: text|json")
	command.Flags().StringVar(&cmdutil.LogLevel, "loglevel", "info", "Set the logging level. One of: debug|info|warn|error")
	command.Flags().IntVar(&glogLevel, "gloglevel", 0, "Set the glog logging level")
	command.Flags().IntVar(&metricsPort, "metrics-port", common.DefaultPortArgoCDMetrics, "Start metrics server on given port")
	command.Flags().DurationVar(&metricsCacheExpiration, "metrics-cache-expiration", 0*time.Second, "Prometheus metrics cache expiration (disabled  by default. e.g. 24h0m0s)")
	command.Flags().IntVar(&selfHealTimeoutSeconds, "self-heal-timeout-seconds", 5, "Specifies timeout between application self heal attempts")
	command.Flags().Int64Var(&kubectlParallelismLimit, "kubectl-parallelism-limit", 20, "Number of allowed concurrent kubectl fork/execs. Any value less the 1 means no limit.")
	command.Flags().BoolVar(&repoServerPlaintext, "repo-server-plaintext", false, "Disable TLS on connections to repo server")
	command.Flags().BoolVar(&repoServerStrictTLS, "repo-server-strict-tls", false, "Whether to use strict validation of the TLS cert presented by the repo server")
	cacheSrc = appstatecache.AddCacheFlagsToCmd(&command, func(client *redis.Client) {
		redisClient = client
	})
	return &command
}

func getClusterFilter() func(cluster *v1alpha1.Cluster) bool {
	replicas := env.ParseNumFromEnv(common.EnvControllerReplicas, 0, 0, math.MaxInt32)
	shard := env.ParseNumFromEnv(common.EnvControllerShard, -1, -math.MaxInt32, math.MaxInt32)
	var clusterFilter func(cluster *v1alpha1.Cluster) bool
	if replicas > 1 {
		if shard < 0 {
			var err error
			shard, err = sharding.InferShard()
			errors.CheckError(err)
		}
		log.Infof("Processing clusters from shard %d", shard)
		clusterFilter = sharding.GetClusterFilter(replicas, shard)
	} else {
		log.Info("Processing all cluster shards")
	}
	return clusterFilter
}
