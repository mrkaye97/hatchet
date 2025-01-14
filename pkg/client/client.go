package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hatchet-dev/hatchet/pkg/client/loader"
	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	"github.com/hatchet-dev/hatchet/pkg/client/types"
	"github.com/hatchet-dev/hatchet/pkg/config/client"
	"github.com/hatchet-dev/hatchet/pkg/logger"
	"github.com/hatchet-dev/hatchet/pkg/validator"
)

type Client interface {
	Admin() AdminClient
	Dispatcher() DispatcherClient
	Event() EventClient
	Subscribe() SubscribeClient
	API() *rest.ClientWithResponses
	TenantId() string
	Namespace() string
}

type clientImpl struct {
	conn *grpc.ClientConn

	admin      AdminClient
	dispatcher DispatcherClient
	event      EventClient
	subscribe  SubscribeClient
	rest       *rest.ClientWithResponses

	// the tenant id
	tenantId string

	namespace string

	l *zerolog.Logger

	v validator.Validator
}

type ClientOpt func(*ClientOpts)

type filesLoaderFunc func() []*types.Workflow

type ClientOpts struct {
	tenantId  string
	l         *zerolog.Logger
	v         validator.Validator
	tls       *tls.Config
	hostPort  string
	serverURL string
	token     string
	namespace string

	filesLoader   filesLoaderFunc
	initWorkflows bool
}

func defaultClientOpts(token *string, cf *client.ClientConfigFile) *ClientOpts {
	var clientConfig *client.ClientConfig
	var err error

	configLoader := &loader.ConfigLoader{}

	if cf == nil {
		// read from environment variables and hostname by default

		clientConfig, err = configLoader.LoadClientConfig(token)

		if err != nil {
			panic(err)
		}

	} else {
		if token != nil {
			cf.Token = *token
		}
		clientConfig, err = loader.GetClientConfigFromConfigFile(cf)

		if err != nil {
			panic(err)
		}
	}

	logger := logger.NewDefaultLogger("client")

	return &ClientOpts{
		tenantId:    clientConfig.TenantId,
		token:       clientConfig.Token,
		l:           &logger,
		v:           validator.NewDefaultValidator(),
		tls:         clientConfig.TLSConfig,
		hostPort:    clientConfig.GRPCBroadcastAddress,
		serverURL:   clientConfig.ServerURL,
		filesLoader: types.DefaultLoader,
		namespace:   clientConfig.Namespace,
	}
}

func WithLogLevel(lvl string) ClientOpt {
	return func(opts *ClientOpts) {
		logger := logger.NewDefaultLogger("client")
		lvl, err := zerolog.ParseLevel(lvl)

		if err == nil {
			logger = logger.Level(lvl)
		}

		opts.l = &logger
	}
}

func WithTenantId(tenantId string) ClientOpt {
	return func(opts *ClientOpts) {
		opts.tenantId = tenantId
	}
}

func WithHostPort(host string, port int) ClientOpt {
	return func(opts *ClientOpts) {
		opts.hostPort = fmt.Sprintf("%s:%d", host, port)
	}
}

func WithToken(token string) ClientOpt {
	return func(opts *ClientOpts) {
		opts.token = token
	}
}

func WithNamespace(namespace string) ClientOpt {
	return func(opts *ClientOpts) {
		opts.namespace = namespace + "_"
	}
}

func InitWorkflows() ClientOpt {
	return func(opts *ClientOpts) {
		opts.initWorkflows = true
	}
}

// WithWorkflows sets the workflow files to use for the worker. If this is not passed in, the workflows files will be loaded
// from the .hatchet folder in the current directory.
func WithWorkflows(files []*types.Workflow) ClientOpt {
	return func(opts *ClientOpts) {
		opts.filesLoader = func() []*types.Workflow {
			return files
		}
	}
}

type sharedClientOpts struct {
	tenantId  string
	namespace string
	l         *zerolog.Logger
	v         validator.Validator
	ctxLoader *contextLoader
}

// New creates a new client instance.
func New(fs ...ClientOpt) (Client, error) {
	var token *string
	initOpts := &ClientOpts{}
	for _, f := range fs {
		f(initOpts)
	}
	if initOpts.token != "" {
		token = &initOpts.token
	}

	opts := defaultClientOpts(token, nil)

	for _, f := range fs {
		f(opts)
	}

	return newFromOpts(opts)
}

func NewFromConfigFile(cf *client.ClientConfigFile, fs ...ClientOpt) (Client, error) {
	opts := defaultClientOpts(nil, cf)

	for _, f := range fs {
		f(opts)
	}

	return newFromOpts(opts)
}

func newFromOpts(opts *ClientOpts) (Client, error) {
	if opts.token == "" {
		return nil, fmt.Errorf("token is required")
	}

	var transportCreds credentials.TransportCredentials

	if opts.tls == nil {
		opts.l.Debug().Msgf("connecting to %s without TLS", opts.hostPort)

		transportCreds = insecure.NewCredentials()
	} else {
		opts.l.Debug().Msgf("connecting to %s with TLS server name %s", opts.hostPort, opts.tls.ServerName)

		transportCreds = credentials.NewTLS(opts.tls)
	}

	grpcOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transportCreds),
	}

	retryOpts := []grpc_retry.CallOption{
		grpc_retry.WithBackoff(grpc_retry.BackoffExponentialWithJitter(5*time.Second, 0.10)),
		grpc_retry.WithMax(5),
		grpc_retry.WithPerRetryTimeout(30 * time.Second),
		grpc_retry.WithCodes(codes.ResourceExhausted, codes.Unavailable),
		grpc_retry.WithOnRetryCallback(grpc_retry.OnRetryCallback(func(ctx context.Context, attempt uint, err error) {
			fmt.Print(ctx, "grpc_retry attempt: %d, backoff for %v", attempt, err)
		})),
	}
	grpcOpts = append(grpcOpts, grpc.WithStreamInterceptor(grpc_retry.StreamClientInterceptor(retryOpts...)))
	grpcOpts = append(grpcOpts, grpc.WithUnaryInterceptor(grpc_retry.UnaryClientInterceptor(retryOpts...)))

	conn, err := grpc.NewClient(
		opts.hostPort,
		grpcOpts...,
	)

	if err != nil {
		return nil, err
	}

	shared := &sharedClientOpts{
		tenantId:  opts.tenantId,
		namespace: opts.namespace,
		l:         opts.l,
		v:         opts.v,
		ctxLoader: newContextLoader(opts.token),
	}

	subscribe := newSubscribe(conn, shared)
	admin := newAdmin(conn, shared, subscribe)
	dispatcher := newDispatcher(conn, shared)
	event := newEvent(conn, shared)

	rest, err := rest.NewClientWithResponses(opts.serverURL, rest.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", opts.token))
		return nil
	}))

	if err != nil {
		return nil, fmt.Errorf("could not create rest client: %w", err)
	}

	// if init workflows is set, then we need to initialize the workflows
	if opts.initWorkflows {
		if err := initWorkflows(opts.filesLoader, admin); err != nil {
			return nil, fmt.Errorf("could not init workflows: %w", err)
		}
	}

	return &clientImpl{
		conn:       conn,
		tenantId:   opts.tenantId,
		l:          opts.l,
		admin:      admin,
		dispatcher: dispatcher,
		subscribe:  subscribe,
		event:      event,
		v:          opts.v,
		rest:       rest,
		namespace:  opts.namespace,
	}, nil
}

func (c *clientImpl) Admin() AdminClient {
	return c.admin
}

func (c *clientImpl) Dispatcher() DispatcherClient {
	return c.dispatcher
}

func (c *clientImpl) Event() EventClient {
	return c.event
}

func (c *clientImpl) Subscribe() SubscribeClient {
	return c.subscribe
}

func (c *clientImpl) API() *rest.ClientWithResponses {
	return c.rest
}

func (c *clientImpl) TenantId() string {
	return c.tenantId
}

func (c *clientImpl) Namespace() string {
	return c.namespace
}

func initWorkflows(fl filesLoaderFunc, adminClient AdminClient) error {
	files := fl()

	for _, file := range files {
		if err := adminClient.PutWorkflow(file); err != nil {
			return fmt.Errorf("could not create workflow: %w", err)
		}
	}

	return nil
}
