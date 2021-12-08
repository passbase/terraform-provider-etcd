package tf6server

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/internal/fromproto"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/internal/tfplugin6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/internal/toproto"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-log/tfsdklog"
	tfaddr "github.com/hashicorp/terraform-registry-address"
	testing "github.com/mitchellh/go-testing-interface"
)

const tflogSubsystemName = "proto"

// Global logging keys attached to all requests.
//
// Practitioners or tooling reading logs may be depending on these keys, so be
// conscious of that when changing them.
const (
	// A unique ID for the RPC request
	logKeyRequestID = "tf_req_id"

	// The full address of the provider, such as
	// registry.terraform.io/hashicorp/random
	logKeyProviderAddress = "tf_provider_addr"

	// The RPC being run, such as "ApplyResourceChange"
	logKeyRPC = "tf_rpc"

	// The type of resource being operated on, such as "random_pet"
	logKeyResourceType = "tf_resource_type"

	// The type of data source being operated on, such as "archive_file"
	logKeyDataSourceType = "tf_data_source_type"

	// The protocol version being used, as a string, such as "6"
	logKeyProtocolVersion = "tf_proto_version"
)

// ServeOpt is an interface for defining options that can be passed to the
// Serve function. Each implementation modifies the ServeConfig being
// generated. A slice of ServeOpts then, cumulatively applied, render a full
// ServeConfig.
type ServeOpt interface {
	ApplyServeOpt(*ServeConfig) error
}

// ServeConfig contains the configured options for how a provider should be
// served.
type ServeConfig struct {
	logger       hclog.Logger
	debugCtx     context.Context
	debugCh      chan *plugin.ReattachConfig
	debugCloseCh chan struct{}

	disableLogInitStderr bool
	disableLogLocation   bool
	useLoggingSink       testing.T
	envVar               string
}

type serveConfigFunc func(*ServeConfig) error

func (s serveConfigFunc) ApplyServeOpt(in *ServeConfig) error {
	return s(in)
}

// WithDebug returns a ServeOpt that will set the server into debug mode, using
// the passed options to populate the go-plugin ServeTestConfig.
func WithDebug(ctx context.Context, config chan *plugin.ReattachConfig, closeCh chan struct{}) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.debugCtx = ctx
		in.debugCh = config
		in.debugCloseCh = closeCh
		return nil
	})
}

// WithGoPluginLogger returns a ServeOpt that will set the logger that
// go-plugin should use to log messages.
func WithGoPluginLogger(logger hclog.Logger) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.logger = logger
		return nil
	})
}

// WithLoggingSink returns a ServeOpt that will enable the logging sink, which
// is used in test frameworks to control where terraform-plugin-log output is
// written and at what levels, mimicking Terraform's logging sink behaviors.
func WithLoggingSink(t testing.T) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.useLoggingSink = t
		return nil
	})
}

// WithoutLogStderrOverride returns a ServeOpt that will disable the
// terraform-plugin-log behavior of logging to the stderr that existed at
// startup, not the stderr that exists when the logging statement is called.
func WithoutLogStderrOverride() ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.disableLogInitStderr = true
		return nil
	})
}

// WithoutLogLocation returns a ServeOpt that will exclude file names and line
// numbers from log output for the terraform-plugin-log logs generated by the
// SDKs and provider.
func WithoutLogLocation() ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.disableLogLocation = true
		return nil
	})
}

// WithLogEnvVarName sets the name of the provider for the purposes of the
// logging environment variable that controls the provider's log level. It is
// the part following TF_LOG_PROVIDER_ and defaults to the name part of the
// provider's registry address, or disabled if it can't parse the provider's
// registry address. Name must only contain letters, numbers, and hyphens.
func WithLogEnvVarName(name string) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		if !regexp.MustCompile(`^[a-zA-Z0-9-]+$`).MatchString(name) {
			return errors.New("environment variable names can only contain a-z, A-Z, 0-9, and -")
		}
		in.envVar = name
		return nil
	})
}

// Serve starts a tfprotov6.ProviderServer serving, ready for Terraform to
// connect to it. The name passed in should be the fully qualified name that
// users will enter in the source field of the required_providers block, like
// "registry.terraform.io/hashicorp/time".
//
// Zero or more options to configure the server may also be passed. The default
// invocation is sufficient, but if the provider wants to run in debug mode or
// modify the logger that go-plugin is using, ServeOpts can be specified to
// support that.
func Serve(name string, serverFactory func() tfprotov6.ProviderServer, opts ...ServeOpt) error {
	var conf ServeConfig
	for _, opt := range opts {
		err := opt.ApplyServeOpt(&conf)
		if err != nil {
			return err
		}
	}
	serveConfig := &plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  6,
			MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
			MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
		},
		Plugins: plugin.PluginSet{
			"provider": &GRPCProviderPlugin{
				GRPCProvider: serverFactory,
				Opts:         opts,
				Name:         name,
			},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	}
	if conf.logger != nil {
		serveConfig.Logger = conf.logger
	}
	if conf.debugCh != nil {
		serveConfig.Test = &plugin.ServeTestConfig{
			Context:          conf.debugCtx,
			ReattachConfigCh: conf.debugCh,
			CloseCh:          conf.debugCloseCh,
		}
	}
	plugin.Serve(serveConfig)
	return nil
}

type server struct {
	downstream tfprotov6.ProviderServer
	tfplugin6.UnimplementedProviderServer

	stopMu sync.Mutex
	stopCh chan struct{}

	tflogSDKOpts tfsdklog.Options
	tflogOpts    tflog.Options
	useTFLogSink bool
	testHandle   testing.T
	name         string
}

func mergeStop(ctx context.Context, cancel context.CancelFunc, stopCh chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-stopCh:
		cancel()
	}
}

// stoppableContext returns a context that wraps `ctx` but will be canceled
// when the server's stopCh is closed.
//
// This is used to cancel all in-flight contexts when the Stop method of the
// server is called.
func (s *server) stoppableContext(ctx context.Context) context.Context {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	stoppable, cancel := context.WithCancel(ctx)
	go mergeStop(stoppable, cancel, s.stopCh)
	return stoppable
}

// loggingContext returns a context that wraps `ctx` and has
// terraform-plugin-log loggers injected.
func (s *server) loggingContext(ctx context.Context) context.Context {
	if s.useTFLogSink {
		ctx = tfsdklog.RegisterTestSink(ctx, s.testHandle)
	}

	// generate a request ID
	reqID, err := uuid.GenerateUUID()
	if err != nil {
		reqID = "unable to assign request ID: " + err.Error()
	}

	// set up the logger SDK loggers are derived from
	ctx = tfsdklog.NewRootSDKLogger(ctx, append(tfsdklog.Options{
		tfsdklog.WithLevelFromEnv("TF_LOG_SDK"),
	}, s.tflogSDKOpts...)...)
	ctx = tfsdklog.With(ctx, logKeyRequestID, reqID)
	ctx = tfsdklog.With(ctx, logKeyProviderAddress, s.name)

	// set up our protocol-level subsystem logger
	ctx = tfsdklog.NewSubsystem(ctx, tflogSubsystemName, append(tfsdklog.Options{
		tfsdklog.WithLevelFromEnv("TF_LOG_SDK_PROTO"),
	}, s.tflogSDKOpts...)...)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyProtocolVersion, "6")

	// set up the provider logger
	ctx = tfsdklog.NewRootProviderLogger(ctx, s.tflogOpts...)
	ctx = tflog.With(ctx, logKeyRequestID, reqID)
	ctx = tflog.With(ctx, logKeyProviderAddress, s.name)
	return ctx
}

func rpcLoggingContext(ctx context.Context, rpc string) context.Context {
	ctx = tfsdklog.With(ctx, logKeyRPC, rpc)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyRPC, rpc)
	ctx = tflog.With(ctx, logKeyRPC, rpc)
	return ctx
}

func resourceLoggingContext(ctx context.Context, resource string) context.Context {
	ctx = tfsdklog.With(ctx, logKeyResourceType, resource)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyResourceType, resource)
	ctx = tflog.With(ctx, logKeyResourceType, resource)
	return ctx
}

func dataSourceLoggingContext(ctx context.Context, dataSource string) context.Context {
	ctx = tfsdklog.With(ctx, logKeyDataSourceType, dataSource)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyDataSourceType, dataSource)
	ctx = tflog.With(ctx, logKeyDataSourceType, dataSource)
	return ctx
}

// New converts a tfprotov6.ProviderServer into a server capable of handling
// Terraform protocol requests and issuing responses using the gRPC types.
func New(name string, serve tfprotov6.ProviderServer, opts ...ServeOpt) tfplugin6.ProviderServer {
	var conf ServeConfig
	for _, opt := range opts {
		err := opt.ApplyServeOpt(&conf)
		if err != nil {
			// this should never happen, we already executed all
			// this code as part of Serve
			panic(err)
		}
	}
	var sdkOptions tfsdklog.Options
	var options tflog.Options
	if !conf.disableLogInitStderr {
		sdkOptions = append(sdkOptions, tfsdklog.WithStderrFromInit())
		options = append(options, tfsdklog.WithStderrFromInit())
	}
	if conf.disableLogLocation {
		sdkOptions = append(sdkOptions, tfsdklog.WithoutLocation())
		options = append(options, tflog.WithoutLocation())
	}
	envVar := conf.envVar
	if envVar == "" {
		addr, err := tfaddr.ParseRawProviderSourceString(name)
		if err != nil {
			log.Printf("[ERROR] Error parsing provider name %q: %s", name, err)
		} else {
			envVar = addr.Type
		}
	}
	envVar = strings.ReplaceAll(envVar, "-", "_")
	if envVar != "" {
		options = append(options, tfsdklog.WithLogName(envVar), tflog.WithLevelFromEnv("TF_LOG_PROVIDER", envVar))
	}
	return &server{
		downstream:   serve,
		stopCh:       make(chan struct{}),
		tflogOpts:    options,
		tflogSDKOpts: sdkOptions,
		name:         name,
		useTFLogSink: conf.useLoggingSink != nil,
		testHandle:   conf.useLoggingSink,
	}
}

func (s *server) GetProviderSchema(ctx context.Context, req *tfplugin6.GetProviderSchema_Request) (*tfplugin6.GetProviderSchema_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "GetProviderSchema")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.GetProviderSchemaRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.GetProviderSchema(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.GetProviderSchema_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ConfigureProvider(ctx context.Context, req *tfplugin6.ConfigureProvider_Request) (*tfplugin6.ConfigureProvider_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "ConfigureProvider")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ConfigureProviderRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ConfigureProvider(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.Configure_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ValidateProviderConfig(ctx context.Context, req *tfplugin6.ValidateProviderConfig_Request) (*tfplugin6.ValidateProviderConfig_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "ValidateProviderConfig")
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ValidateProviderConfigRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ValidateProviderConfig(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ValidateProviderConfig_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

// stop closes the stopCh associated with the server and replaces it with a new
// one.
//
// This causes all in-flight requests for the server to have their contexts
// canceled.
func (s *server) stop() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	close(s.stopCh)
	s.stopCh = make(chan struct{})
}

func (s *server) Stop(ctx context.Context, req *tfplugin6.StopProvider_Request) (*tfplugin6.StopProvider_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "Stop")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.StopProviderRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.StopProvider(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Closing all our contexts")
	s.stop()
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Closed all our contexts")
	ret, err := toproto.Stop_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ValidateDataResourceConfig(ctx context.Context, req *tfplugin6.ValidateDataResourceConfig_Request) (*tfplugin6.ValidateDataResourceConfig_Response, error) {
	ctx = dataSourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ValidateDataResourceConfig"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ValidateDataResourceConfigRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ValidateDataResourceConfig(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ValidateDataResourceConfig_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ReadDataSource(ctx context.Context, req *tfplugin6.ReadDataSource_Request) (*tfplugin6.ReadDataSource_Response, error) {
	ctx = dataSourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ReadDataSource"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ReadDataSourceRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ReadDataSource(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ReadDataSource_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ValidateResourceConfig(ctx context.Context, req *tfplugin6.ValidateResourceConfig_Request) (*tfplugin6.ValidateResourceConfig_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ValidateResourceConfig"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ValidateResourceConfigRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ValidateResourceConfig(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ValidateResourceConfig_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) UpgradeResourceState(ctx context.Context, req *tfplugin6.UpgradeResourceState_Request) (*tfplugin6.UpgradeResourceState_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "UpgradeResourceState"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.UpgradeResourceStateRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.UpgradeResourceState(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.UpgradeResourceState_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ReadResource(ctx context.Context, req *tfplugin6.ReadResource_Request) (*tfplugin6.ReadResource_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ReadResource"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ReadResourceRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ReadResource(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ReadResource_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfplugin6.PlanResourceChange_Request) (*tfplugin6.PlanResourceChange_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "PlanResourceChange"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.PlanResourceChangeRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.PlanResourceChange(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.PlanResourceChange_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfplugin6.ApplyResourceChange_Request) (*tfplugin6.ApplyResourceChange_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ApplyResourceChange"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ApplyResourceChangeRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ApplyResourceChange(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ApplyResourceChange_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ImportResourceState(ctx context.Context, req *tfplugin6.ImportResourceState_Request) (*tfplugin6.ImportResourceState_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ImportResourceState"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ImportResourceStateRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ImportResourceState(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ImportResourceState_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}
