// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Every change should be reflected on help.go as well.

package etcdmain

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"runtime"
	"strings"

	"go.etcd.io/etcd/embed"
	"go.etcd.io/etcd/pkg/flags"
	"go.etcd.io/etcd/pkg/logutil"
	"go.etcd.io/etcd/pkg/types"
	"go.etcd.io/etcd/version"

	"go.uber.org/zap"
	"sigs.k8s.io/yaml"
)

var (
	proxyFlagOff      = "off"
	proxyFlagReadonly = "readonly"
	proxyFlagOn       = "on"

	fallbackFlagExit  = "exit"
	fallbackFlagProxy = "proxy"

	ignored = []string{
		"cluster-active-size",
		"cluster-remove-delay",
		"cluster-sync-interval",
		"config",
		"force",
		"max-result-buffer",
		"max-retry-attempts",
		"peer-heartbeat-interval",
		"peer-election-timeout",
		"retry-interval",
		"snapshot",
		"v",
		"vv",
		// for coverage testing
		"test.coverprofile",
		"test.outputdir",
	}
)

type configProxy struct {
	ProxyFailureWaitMs     uint `json:"proxy-failure-wait"`
	ProxyRefreshIntervalMs uint `json:"proxy-refresh-interval"`
	ProxyDialTimeoutMs     uint `json:"proxy-dial-timeout"`
	ProxyWriteTimeoutMs    uint `json:"proxy-write-timeout"`
	ProxyReadTimeoutMs     uint `json:"proxy-read-timeout"`
	Fallback               string
	Proxy                  string
	ProxyJSON              string `json:"proxy"`
	FallbackJSON           string `json:"discovery-fallback"`
}

// config holds the config for a command line invocation of etcd
type config struct {
	ec           embed.Config
	cp           configProxy
	cf           configFlags
	configFile   string
	printVersion bool
	ignored      []string
}

// configFlags has the set of flags used for command line parsing a Config
type configFlags struct {
	flagSet      *flag.FlagSet
	clusterState *flags.SelectiveStringValue
	fallback     *flags.SelectiveStringValue
	proxy        *flags.SelectiveStringValue
}

func newConfig() *config {
	// Tip: 这里生成的Config都是采用默认值的
	cfg := &config{
		ec: *embed.NewConfig(),
		cp: configProxy{
			Proxy:                  proxyFlagOff,
			ProxyFailureWaitMs:     5000,
			ProxyRefreshIntervalMs: 30000,
			ProxyDialTimeoutMs:     1000,
			ProxyWriteTimeoutMs:    5000,
		},
		ignored: ignored,
	}
	cfg.cf = configFlags{
		flagSet: flag.NewFlagSet("etcd", flag.ContinueOnError),
		clusterState: flags.NewSelectiveStringValue(
			embed.ClusterStateFlagNew,
			embed.ClusterStateFlagExisting,
		),
		fallback: flags.NewSelectiveStringValue(
			fallbackFlagProxy,
			fallbackFlagExit,
		),
		proxy: flags.NewSelectiveStringValue(
			proxyFlagOff,
			proxyFlagReadonly,
			proxyFlagOn,
		),
	}

	// Tip: 输入的命令行不符合规则时，打印出对应的使用方法,自定义的参数都是在这里传进来的
	// Tip: flag包很难满足复杂场景，这里不得不提一个package：github.com/spf13/cobra，在k8s等其余项目中也有大量应用
	fs := cfg.cf.flagSet
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, usageline)
	}

	fs.StringVar(&cfg.configFile, "config-file", "", "Path to the server configuration file. Note that if a configuration file is provided, other command line flags and environment variables will be ignored.")

	// Tip: 自身相关的参数
	// member
	fs.StringVar(&cfg.ec.Dir, "data-dir", cfg.ec.Dir, "Path to the data directory.")
	fs.StringVar(&cfg.ec.WalDir, "wal-dir", cfg.ec.WalDir, "Path to the dedicated wal directory.")
	// Tip: peer是同伴的意思，所以这个url是给etcd之间通信用的
	fs.Var(
		flags.NewUniqueURLsWithExceptions(embed.DefaultListenPeerURLs, ""),
		"listen-peer-urls",
		"List of URLs to listen on for peer traffic.",
	)
	// Tip: 这个url是给客户端通信用的
	fs.Var(
		flags.NewUniqueURLsWithExceptions(embed.DefaultListenClientURLs, ""),
		"listen-client-urls",
		"List of URLs to listen on for client traffic.",
	)
	// Tip: metrics是prometheus监控采集的原始数据，用于监控，可以看到默认不开启
	fs.Var(
		flags.NewUniqueURLsWithExceptions("", ""),
		"listen-metrics-urls",
		"List of URLs to listen on for the metrics and health endpoints.",
	)
	// Tip: 下面的参数，现根据字面意思，大致理解下，后续到具体使用时再了解
	// snapshot保存的最大数量
	fs.UintVar(&cfg.ec.MaxSnapFiles, "max-snapshots", cfg.ec.MaxSnapFiles, "Maximum number of snapshot files to retain (0 is unlimited).")
	// wals文件的最大数量
	fs.UintVar(&cfg.ec.MaxWalFiles, "max-wals", cfg.ec.MaxWalFiles, "Maximum number of wal files to retain (0 is unlimited).")
	// 给这个etcd起别名，方便阅读
	fs.StringVar(&cfg.ec.Name, "name", cfg.ec.Name, "Human-readable name for this member.")
	// 提交一定次数后，触发保存快照,默认100000
	fs.Uint64Var(&cfg.ec.SnapshotCount, "snapshot-count", cfg.ec.SnapshotCount, "Number of committed transactions to trigger a snapshot to disk.")
	// 心跳频率ms，默认100ms
	fs.UintVar(&cfg.ec.TickMs, "heartbeat-interval", cfg.ec.TickMs, "Time (in milliseconds) of a heartbeat interval.")
	// 超过这个时间后，进行重新选举，默认1000ms
	fs.UintVar(&cfg.ec.ElectionMs, "election-timeout", cfg.ec.ElectionMs, "Time (in milliseconds) for an election to timeout.")
	// 加速初次选举leader的超时，主要适合跨数据中心或大型集群的场景，比如设置超时为10s，会在等待8s后就开始选举
	fs.BoolVar(&cfg.ec.InitialElectionTickAdvance, "initial-election-tick-advance", cfg.ec.InitialElectionTickAdvance, "Whether to fast-forward initial election ticks on boot for faster election.")
	// 后端超过指定大小后告警，默认为2 * 1024 * 1024 * 1024=2GB
	fs.Int64Var(&cfg.ec.QuotaBackendBytes, "quota-backend-bytes", cfg.ec.QuotaBackendBytes, "Raise alarms when backend size exceeds the given quota. 0 means use the default quota.")
	// 后端事务的刷盘flush的间隔，默认 100 * time.Millisecond
	fs.DurationVar(&cfg.ec.BackendBatchInterval, "backend-batch-interval", cfg.ec.BackendBatchInterval, "BackendBatchInterval is the maximum time before commit the backend transaction.")
	// 后端事务执行多少次后刷盘，默认10000
	fs.IntVar(&cfg.ec.BackendBatchLimit, "backend-batch-limit", cfg.ec.BackendBatchLimit, "BackendBatchLimit is the maximum operations before commit the backend transaction.")
	// 一个事务中最多可以执行多少个指令，默认10000
	fs.UintVar(&cfg.ec.MaxTxnOps, "max-txn-ops", cfg.ec.MaxTxnOps, "Maximum number of operations permitted in a transaction.")
	// 客户端请求的大小，默认1.5 * 1024 * 1024=1.5MB
	fs.UintVar(&cfg.ec.MaxRequestBytes, "max-request-bytes", cfg.ec.MaxRequestBytes, "Maximum client request size in bytes the server will accept.")
	// gRPC KeepAlive 的最短等待时间，默认5s
	fs.DurationVar(&cfg.ec.GRPCKeepAliveMinTime, "grpc-keepalive-min-time", cfg.ec.GRPCKeepAliveMinTime, "Minimum interval duration that a client should wait before pinging server.")
	// gRPC KeepAlive 对连接探活的周期，默认2h
	fs.DurationVar(&cfg.ec.GRPCKeepAliveInterval, "grpc-keepalive-interval", cfg.ec.GRPCKeepAliveInterval, "Frequency duration of server-to-client ping to check if a connection is alive (0 to disable).")
	// gRPC KeepAlive 的超时时间，默认20s
	fs.DurationVar(&cfg.ec.GRPCKeepAliveTimeout, "grpc-keepalive-timeout", cfg.ec.GRPCKeepAliveTimeout, "Additional duration of wait before closing a non-responsive connection (0 to disable).")
	
	// Tip: 集群相关的参数
	// clustering
	// Tip: initial-advertise-peer-urls集群所有节点的监听peer的端口
	fs.Var(
		flags.NewUniqueURLsWithExceptions(embed.DefaultInitialAdvertisePeerURLs, ""),
		"initial-advertise-peer-urls",
		"List of this member's peer URLs to advertise to the rest of the cluster.",
	)
	// Tip: advertise-client-urls集群所有节点的监听client的端口
	fs.Var(
		flags.NewUniqueURLsWithExceptions(embed.DefaultAdvertiseClientURLs, ""),
		"advertise-client-urls",
		"List of this member's client URLs to advertise to the public.",
	)
	// Tip: 通过服务发现方式启动ETCD集群，依赖另一个etcd集群，下面这些参数到具体实现时再分析
	fs.StringVar(&cfg.ec.Durl, "discovery", cfg.ec.Durl, "Discovery URL used to bootstrap the cluster.")
	fs.Var(cfg.cf.fallback, "discovery-fallback", fmt.Sprintf("Valid values include %q", cfg.cf.fallback.Valids()))

	fs.StringVar(&cfg.ec.Dproxy, "discovery-proxy", cfg.ec.Dproxy, "HTTP proxy to use for traffic to discovery service.")
	fs.StringVar(&cfg.ec.DNSCluster, "discovery-srv", cfg.ec.DNSCluster, "DNS domain used to bootstrap initial cluster.")
	fs.StringVar(&cfg.ec.DNSClusterServiceName, "discovery-srv-name", cfg.ec.DNSClusterServiceName, "Service name to query when using DNS discovery.")
	fs.StringVar(&cfg.ec.InitialCluster, "initial-cluster", cfg.ec.InitialCluster, "Initial cluster configuration for bootstrapping.")
	fs.StringVar(&cfg.ec.InitialClusterToken, "initial-cluster-token", cfg.ec.InitialClusterToken, "Initial cluster token for the etcd cluster during bootstrap.")
	fs.Var(cfg.cf.clusterState, "initial-cluster-state", "Initial cluster state ('new' or 'existing').")

	// 严格控制重新加载配置
	fs.BoolVar(&cfg.ec.StrictReconfigCheck, "strict-reconfig-check", cfg.ec.StrictReconfigCheck, "Reject reconfiguration requests that would cause quorum loss.")
	// 是否兼容v2的客户端请求，默认关闭
	fs.BoolVar(&cfg.ec.EnableV2, "enable-v2", cfg.ec.EnableV2, "Accept etcd V2 client requests.")
	// 在投票前先进行一轮检查，看是否满足选举的最小数量要求，在3.5后会默认开启
	fs.BoolVar(&cfg.ec.PreVote, "pre-vote", cfg.ec.PreVote, "Enable to run an additional Raft election phase.")

	// Tip: etcd的代理相关参数，作为反向代理到其它etcd，有只读和读写两种模式
	// proxy
	// proxy相关参数暂不细看，主要是一些等待、超时相关的参数
	fs.Var(cfg.cf.proxy, "proxy", fmt.Sprintf("Valid values include %q", cfg.cf.proxy.Valids()))
	fs.UintVar(&cfg.cp.ProxyFailureWaitMs, "proxy-failure-wait", cfg.cp.ProxyFailureWaitMs, "Time (in milliseconds) an endpoint will be held in a failed state.")
	fs.UintVar(&cfg.cp.ProxyRefreshIntervalMs, "proxy-refresh-interval", cfg.cp.ProxyRefreshIntervalMs, "Time (in milliseconds) of the endpoints refresh interval.")
	fs.UintVar(&cfg.cp.ProxyDialTimeoutMs, "proxy-dial-timeout", cfg.cp.ProxyDialTimeoutMs, "Time (in milliseconds) for a dial to timeout.")
	fs.UintVar(&cfg.cp.ProxyWriteTimeoutMs, "proxy-write-timeout", cfg.cp.ProxyWriteTimeoutMs, "Time (in milliseconds) for a write to timeout.")
	fs.UintVar(&cfg.cp.ProxyReadTimeoutMs, "proxy-read-timeout", cfg.cp.ProxyReadTimeoutMs, "Time (in milliseconds) for a read to timeout.")

	// Tip: 安全相关的参数，包括证书之类的，暂不细看
	// security
	fs.StringVar(&cfg.ec.ClientTLSInfo.CertFile, "cert-file", "", "Path to the client server TLS cert file.")
	fs.StringVar(&cfg.ec.ClientTLSInfo.KeyFile, "key-file", "", "Path to the client server TLS key file.")
	fs.BoolVar(&cfg.ec.ClientTLSInfo.ClientCertAuth, "client-cert-auth", false, "Enable client cert authentication.")
	fs.StringVar(&cfg.ec.ClientTLSInfo.CRLFile, "client-crl-file", "", "Path to the client certificate revocation list file.")
	fs.StringVar(&cfg.ec.ClientTLSInfo.AllowedHostname, "client-cert-allowed-hostname", "", "Allowed TLS hostname for client cert authentication.")
	fs.StringVar(&cfg.ec.ClientTLSInfo.TrustedCAFile, "trusted-ca-file", "", "Path to the client server TLS trusted CA cert file.")
	fs.BoolVar(&cfg.ec.ClientAutoTLS, "auto-tls", false, "Client TLS using generated certificates")
	fs.StringVar(&cfg.ec.PeerTLSInfo.CertFile, "peer-cert-file", "", "Path to the peer server TLS cert file.")
	fs.StringVar(&cfg.ec.PeerTLSInfo.KeyFile, "peer-key-file", "", "Path to the peer server TLS key file.")
	fs.BoolVar(&cfg.ec.PeerTLSInfo.ClientCertAuth, "peer-client-cert-auth", false, "Enable peer client cert authentication.")
	fs.StringVar(&cfg.ec.PeerTLSInfo.TrustedCAFile, "peer-trusted-ca-file", "", "Path to the peer server TLS trusted CA file.")
	fs.BoolVar(&cfg.ec.PeerAutoTLS, "peer-auto-tls", false, "Peer TLS using generated certificates")
	fs.StringVar(&cfg.ec.PeerTLSInfo.CRLFile, "peer-crl-file", "", "Path to the peer certificate revocation list file.")
	fs.StringVar(&cfg.ec.PeerTLSInfo.AllowedCN, "peer-cert-allowed-cn", "", "Allowed CN for inter peer authentication.")
	fs.StringVar(&cfg.ec.PeerTLSInfo.AllowedHostname, "peer-cert-allowed-hostname", "", "Allowed TLS hostname for inter peer authentication.")
	fs.Var(flags.NewStringsValue(""), "cipher-suites", "Comma-separated list of supported TLS cipher suites between client/server and peers (empty will be auto-populated by Go).")
	fs.BoolVar(&cfg.ec.PeerTLSInfo.SkipClientSANVerify, "experimental-peer-skip-client-san-verification", false, "Skip verification of SAN field in client certificate for peer connections.")

	fs.Var(
		flags.NewUniqueURLsWithExceptions("*", "*"),
		"cors",
		"Comma-separated white list of origins for CORS, or cross-origin resource sharing, (empty or * means allow all)",
	)
	fs.Var(flags.NewUniqueStringsValue("*"), "host-whitelist", "Comma-separated acceptable hostnames from HTTP client requests, if server is not secure (empty means allow all).")

	// Tip: 日志打印默认info级别，3.5之后会有大量变动
	// logging
	fs.StringVar(&cfg.ec.Logger, "logger", "capnslog", "Specify 'zap' for structured logging or 'capnslog'. WARN: 'capnslog' is being deprecated in v3.5.")
	fs.Var(flags.NewUniqueStringsValue(embed.DefaultLogOutput), "log-output", "[TO BE DEPRECATED IN v3.5] use '--log-outputs'.")
	fs.Var(flags.NewUniqueStringsValue(embed.DefaultLogOutput), "log-outputs", "Specify 'stdout' or 'stderr' to skip journald logging even when running under systemd, or list of comma separated output targets.")
	fs.BoolVar(&cfg.ec.Debug, "debug", false, "[TO BE DEPRECATED IN v3.5] Enable debug-level logging for etcd. Use '--log-level=debug' instead.")
	fs.StringVar(&cfg.ec.LogLevel, "log-level", logutil.DefaultLogLevel, "Configures log level. Only supports debug, info, warn, error, panic, or fatal. Default 'info'.")
	fs.StringVar(&cfg.ec.LogPkgLevels, "log-package-levels", "", "[TO BE DEPRECATED IN v3.5] Specify a particular log level for each etcd package (eg: 'etcdmain=CRITICAL,etcdserver=DEBUG').")

	// version
	// 打印版本并退出
	fs.BoolVar(&cfg.printVersion, "version", false, "Print the version and exit.")

	// 压缩归档的参数
	fs.StringVar(&cfg.ec.AutoCompactionRetention, "auto-compaction-retention", "0", "Auto compaction retention for mvcc key value store. 0 means disable auto compaction.")
	fs.StringVar(&cfg.ec.AutoCompactionMode, "auto-compaction-mode", "periodic", "interpret 'auto-compaction-retention' one of: periodic|revision. 'periodic' for duration based retention, defaulting to hours if no time unit is provided (e.g. '5m'). 'revision' for revision number based retention.")

	// pprof profiler via HTTP
	// 用pprof进行性能监控
	fs.BoolVar(&cfg.ec.EnablePprof, "enable-pprof", false, "Enable runtime profiling data via HTTP server. Address is at client URL + \"/debug/pprof/\"")

	// additional metrics
	// 监控metrics，默认basic，可以设置为extensive来增加histogram图，用于统计
	fs.StringVar(&cfg.ec.Metrics, "metrics", cfg.ec.Metrics, "Set level of detail for exported metrics, specify 'extensive' to include histogram metrics")

	// auth
	// 认证参数
	fs.StringVar(&cfg.ec.AuthToken, "auth-token", cfg.ec.AuthToken, "Specify auth token specific options.")
	fs.UintVar(&cfg.ec.BcryptCost, "bcrypt-cost", cfg.ec.BcryptCost, "Specify bcrypt algorithm cost factor for auth password hashing.")

	// gateway
	// 开启gRPC网关
	fs.BoolVar(&cfg.ec.EnableGRPCGateway, "enable-grpc-gateway", true, "Enable GRPC gateway.")

	// 实验相关的参数，暂时不分析
	// experimental
	fs.BoolVar(&cfg.ec.ExperimentalInitialCorruptCheck, "experimental-initial-corrupt-check", cfg.ec.ExperimentalInitialCorruptCheck, "Enable to check data corruption before serving any client/peer traffic.")
	fs.DurationVar(&cfg.ec.ExperimentalCorruptCheckTime, "experimental-corrupt-check-time", cfg.ec.ExperimentalCorruptCheckTime, "Duration of time between cluster corruption check passes.")
	fs.StringVar(&cfg.ec.ExperimentalEnableV2V3, "experimental-enable-v2v3", cfg.ec.ExperimentalEnableV2V3, "v3 prefix for serving emulated v2 state.")
	fs.StringVar(&cfg.ec.ExperimentalBackendFreelistType, "experimental-backend-bbolt-freelist-type", cfg.ec.ExperimentalBackendFreelistType, "ExperimentalBackendFreelistType specifies the type of freelist that boltdb backend uses(array and map are supported types)")
	fs.BoolVar(&cfg.ec.ExperimentalEnableLeaseCheckpoint, "experimental-enable-lease-checkpoint", false, "Enable to persist lease remaining TTL to prevent indefinite auto-renewal of long lived leases.")
	fs.IntVar(&cfg.ec.ExperimentalCompactionBatchLimit, "experimental-compaction-batch-limit", cfg.ec.ExperimentalCompactionBatchLimit, "Sets the maximum revisions deleted in each compaction batch.")

	// 不安全的参数
	// unsafe
	fs.BoolVar(&cfg.ec.UnsafeNoFsync, "unsafe-no-fsync", false, "Disables fsync, unsafe, will cause data loss.")
	fs.BoolVar(&cfg.ec.ForceNewCluster, "force-new-cluster", false, "Force to create a new one member cluster.")

	// 即已经弃用的flag，会提示已经弃用，程序仍继续运行
	// ignored
	for _, f := range cfg.ignored {
		fs.Var(&flags.IgnoredFlag{Name: f}, f, "")
	}
	return cfg
}

func (cfg *config) parse(arguments []string) error {
	// 解析flag参数
	perr := cfg.cf.flagSet.Parse(arguments)
	switch perr {
	case nil:
	case flag.ErrHelp:
		fmt.Println(flagsline)
		os.Exit(0)
	default:
		os.Exit(2)
	}
	if len(cfg.cf.flagSet.Args()) != 0 {
		return fmt.Errorf("'%s' is not a valid flag", cfg.cf.flagSet.Arg(0))
	}

	if cfg.printVersion {
		fmt.Printf("etcd Version: %s\n", version.Version)
		fmt.Printf("Git SHA: %s\n", version.GitSHA)
		fmt.Printf("Go Version: %s\n", runtime.Version())
		fmt.Printf("Go OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	var err error

	// This env variable must be parsed separately
	// because we need to determine whether to use or
	// ignore the env variables based on if the config file is set.
	// 加载配置文件，如果没有设置，会去读取环境变量 ETCD_CONFIG_FILE
	if cfg.configFile == "" {
		cfg.configFile = os.Getenv(flags.FlagToEnv("ETCD", "config-file"))
	}

	// Tip: 如果有配置文件，那么就从配置文件中加载；否则就从命令行加载
	if cfg.configFile != "" {
		err = cfg.configFromFile(cfg.configFile)
		if lg := cfg.ec.GetLogger(); lg != nil {
			lg.Info(
				"loaded server configuration, other configuration command line flags and environment variables will be ignored if provided",
				zap.String("path", cfg.configFile),
			)
		} else {
			plog.Infof("Loading server configuration from %q. Other configuration command line flags and environment variables will be ignored if provided.", cfg.configFile)
		}
	} else {
		err = cfg.configFromCmdLine()
	}
	// now logger is set up
	return err
}

func (cfg *config) configFromCmdLine() error {
	err := flags.SetFlagsFromEnv("ETCD", cfg.cf.flagSet)
	if err != nil {
		return err
	}

	cfg.ec.LPUrls = flags.UniqueURLsFromFlag(cfg.cf.flagSet, "listen-peer-urls")
	cfg.ec.APUrls = flags.UniqueURLsFromFlag(cfg.cf.flagSet, "initial-advertise-peer-urls")
	cfg.ec.LCUrls = flags.UniqueURLsFromFlag(cfg.cf.flagSet, "listen-client-urls")
	cfg.ec.ACUrls = flags.UniqueURLsFromFlag(cfg.cf.flagSet, "advertise-client-urls")
	cfg.ec.ListenMetricsUrls = flags.UniqueURLsFromFlag(cfg.cf.flagSet, "listen-metrics-urls")

	cfg.ec.CORS = flags.UniqueURLsMapFromFlag(cfg.cf.flagSet, "cors")
	cfg.ec.HostWhitelist = flags.UniqueStringsMapFromFlag(cfg.cf.flagSet, "host-whitelist")

	cfg.ec.CipherSuites = flags.StringsFromFlag(cfg.cf.flagSet, "cipher-suites")

	// TODO: remove this in v3.5
	cfg.ec.DeprecatedLogOutput = flags.UniqueStringsFromFlag(cfg.cf.flagSet, "log-output")
	cfg.ec.LogOutputs = flags.UniqueStringsFromFlag(cfg.cf.flagSet, "log-outputs")

	cfg.ec.ClusterState = cfg.cf.clusterState.String()
	cfg.cp.Fallback = cfg.cf.fallback.String()
	cfg.cp.Proxy = cfg.cf.proxy.String()

	// disable default advertise-client-urls if lcurls is set
	missingAC := flags.IsSet(cfg.cf.flagSet, "listen-client-urls") && !flags.IsSet(cfg.cf.flagSet, "advertise-client-urls")
	if !cfg.mayBeProxy() && missingAC {
		cfg.ec.ACUrls = nil
	}

	// disable default initial-cluster if discovery is set
	if (cfg.ec.Durl != "" || cfg.ec.DNSCluster != "" || cfg.ec.DNSClusterServiceName != "") && !flags.IsSet(cfg.cf.flagSet, "initial-cluster") {
		cfg.ec.InitialCluster = ""
	}

	return cfg.validate()
}

func (cfg *config) configFromFile(path string) error {
	eCfg, err := embed.ConfigFromFile(path)
	if err != nil {
		return err
	}
	cfg.ec = *eCfg

	// load extra config information
	b, rerr := ioutil.ReadFile(path)
	if rerr != nil {
		return rerr
	}
	if yerr := yaml.Unmarshal(b, &cfg.cp); yerr != nil {
		return yerr
	}

	if cfg.ec.ListenMetricsUrlsJSON != "" {
		us, err := types.NewURLs(strings.Split(cfg.ec.ListenMetricsUrlsJSON, ","))
		if err != nil {
			log.Fatalf("unexpected error setting up listen-metrics-urls: %v", err)
		}
		cfg.ec.ListenMetricsUrls = []url.URL(us)
	}

	if cfg.cp.FallbackJSON != "" {
		if err := cfg.cf.fallback.Set(cfg.cp.FallbackJSON); err != nil {
			log.Fatalf("unexpected error setting up discovery-fallback flag: %v", err)
		}
		cfg.cp.Fallback = cfg.cf.fallback.String()
	}

	if cfg.cp.ProxyJSON != "" {
		if err := cfg.cf.proxy.Set(cfg.cp.ProxyJSON); err != nil {
			log.Fatalf("unexpected error setting up proxyFlag: %v", err)
		}
		cfg.cp.Proxy = cfg.cf.proxy.String()
	}
	return nil
}

func (cfg *config) mayBeProxy() bool {
	mayFallbackToProxy := cfg.ec.Durl != "" && cfg.cp.Fallback == fallbackFlagProxy
	return cfg.cp.Proxy != proxyFlagOff || mayFallbackToProxy
}

func (cfg *config) validate() error {
	err := cfg.ec.Validate()
	// TODO(yichengq): check this for joining through discovery service case
	if err == embed.ErrUnsetAdvertiseClientURLsFlag && cfg.mayBeProxy() {
		return nil
	}
	return err
}

func (cfg config) isProxy() bool               { return cfg.cf.proxy.String() != proxyFlagOff }
func (cfg config) isReadonlyProxy() bool       { return cfg.cf.proxy.String() == proxyFlagReadonly }
func (cfg config) shouldFallbackToProxy() bool { return cfg.cf.fallback.String() == fallbackFlagProxy }
