package terminal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/macaroons"
	grpcProxy "github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

const (
	contentTypeGrpc = "application/grpc"

	// HeaderMacaroon is the HTTP header field name that is used to send
	// the macaroon.
	HeaderMacaroon = "Macaroon"
)

// newRpcProxy creates a new RPC proxy that can take any native gRPC, grpc-web
// or REST request and delegate (and convert if necessary) it to the correct
// component.
func newRpcProxy(cfg *Config, validator macaroons.MacaroonValidator,
	permissionMap map[string][]bakery.Op) *rpcProxy {

	// The gRPC web calls are protected by HTTP basic auth which is defined
	// by base64(username:password). Because we only have a password, we
	// just use base64(password:password).
	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		"%s:%s", cfg.UIPassword, cfg.UIPassword,
	)))

	// Set up the final gRPC server that will serve gRPC web to the browser
	// and translate all incoming gRPC web calls into native gRPC that are
	// then forwarded to lnd's RPC interface. GRPC web has a few kinks that
	// need to be addressed with a custom director that just takes care of a
	// few HTTP header fields.
	p := &rpcProxy{
		cfg:            cfg,
		basicAuth:      basicAuth,
		macValidator:   validator,
		lndConnectChan: make(chan struct{}),
	}
	p.grpcServer = grpc.NewServer(
		// From the grpxProxy doc: This codec is *crucial* to the
		// functioning of the proxy.
		grpc.CustomCodec(grpcProxy.Codec()),
		grpc.ChainStreamInterceptor(p.StreamServerInterceptor(
			permissionMap,
		)),
		grpc.ChainUnaryInterceptor(p.UnaryServerInterceptor(
			permissionMap,
		)),
		grpc.UnknownServiceHandler(
			grpcProxy.TransparentHandler(p.director),
		),
	)

	// Create the gRPC web proxy that wraps the just created grpcServer and
	// converts the browser's gRPC web calls into native gRPC.
	options := []grpcweb.Option{
		grpcweb.WithWebsockets(true),
		grpcweb.WithWebsocketPingInterval(2 * time.Minute),
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
	}
	p.grpcWebProxy = grpcweb.WrapServer(p.grpcServer, options...)
	return p
}

// rpcProxy is an RPC proxy server that can take any native gRPC, grpc-web or
// REST request and delegate it to the correct component. Any grpc-web request
// is first converted into a native gRPC request. The gRPC call is then handed
// to our local gRPC server that has all in-process RPC servers registered. If
// the call is meant for a component that is registered as running in-process,
// it is handled there. If not, the director will forward the call to either a
// local or remote lnd instance.
//
//    any RPC or grpc-web call
//        |
//        V
//    +---+----------------------+
//    | grpc-web proxy           |
//    +---+----------------------+
//        |
//        v native gRPC call with basic auth
//    +---+----------------------+
//    | interceptors             |
//    +---+----------------------+
//        |
//        v native gRPC call with macaroon
//    +---+----------------------+
//    | gRPC server              |
//    +---+----------------------+
//        |
//        v unknown authenticated call, gRPC server is just a wrapper
//    +---+----------------------+
//    | director                 |
//    +---+----------------------+
//        |
//        v authenticated call
//    +---+----------------------+ call to lnd or integrated daemon
//    | lnd (remote or local)    +---------------+
//    | faraday remote           |               |
//    | loop remote              |    +----------v----------+
//    | pool remote              |    | lnd local subserver |
//    +--------------------------+    |  - faraday          |
//                                    |  - loop             |
//                                    |  - pool             |
//                                    +---------------------+
//
type rpcProxy struct {
	cfg       *Config
	basicAuth string

	macValidator macaroons.MacaroonValidator

	lndConn     *grpc.ClientConn
	faradayConn *grpc.ClientConn
	loopConn    *grpc.ClientConn
	poolConn    *grpc.ClientConn

	grpcServer     *grpc.Server
	grpcWebProxy   *grpcweb.WrappedGrpcServer
	lndConnectChan chan struct{}
	lndConnectSent bool
}

// Start creates initial connection to lnd.
func (p *rpcProxy) Start() error {
	var err error

	// If we're in stateless remote mode, we need to wait until the user
	// passes in the LND connection info into the UI before connecting to
	// LND.
	if p.cfg.LndMode != ModeStatelessRemote {
		// Setup the connection to lnd.
		host, _, tlsPath, _ := p.cfg.lndConnectParams()
		p.lndConn, err = dialBackend("lnd", host, tlsPath, "")
		if err != nil {
			return fmt.Errorf("could not dial lnd: %v", err)
		}
	}

	// Make sure we can connect to all the daemons that are configured to be
	// running in remote mode.
	if p.cfg.faradayRemote {
		p.faradayConn, err = dialBackend(
			"faraday", p.cfg.Remote.Faraday.RPCServer,
			lncfg.CleanAndExpandPath(
				p.cfg.Remote.Faraday.TLSCertPath,
			), "",
		)
		if err != nil {
			return fmt.Errorf("could not dial remote faraday: %v",
				err)
		}
	}

	if p.cfg.loopRemote {
		p.loopConn, err = dialBackend(
			"loop", p.cfg.Remote.Loop.RPCServer,
			lncfg.CleanAndExpandPath(p.cfg.Remote.Loop.TLSCertPath),
			"",
		)
		if err != nil {
			return fmt.Errorf("could not dial remote loop: %v", err)
		}
	}

	if p.cfg.poolRemote {
		p.poolConn, err = dialBackend(
			"pool", p.cfg.Remote.Pool.RPCServer,
			lncfg.CleanAndExpandPath(p.cfg.Remote.Pool.TLSCertPath),
			"",
		)
		if err != nil {
			return fmt.Errorf("could not dial remote pool: %v", err)
		}
	}

	return nil
}

// Stop shuts down the lnd connection.
func (p *rpcProxy) Stop() error {
	p.grpcServer.Stop()

	if p.lndConn != nil {
		if err := p.lndConn.Close(); err != nil {
			log.Errorf("Error closing lnd connection: %v", err)
			return err
		}
	}

	if p.faradayConn != nil {
		if err := p.faradayConn.Close(); err != nil {
			log.Errorf("Error closing faraday connection: %v", err)
			return err
		}
	}

	if p.loopConn != nil {
		if err := p.loopConn.Close(); err != nil {
			log.Errorf("Error closing loop connection: %v", err)
			return err
		}
	}

	if p.poolConn != nil {
		if err := p.poolConn.Close(); err != nil {
			log.Errorf("Error closing pool connection: %v", err)
			return err
		}
	}

	return nil
}

// isHandling checks if the specified request is something to be handled by lnd
// or any of the attached sub daemons. If true is returned, the call was handled
// by the RPC proxy and the caller MUST NOT handle it again. If false is
// returned, the request was not handled and the caller MUST handle it.
func (p *rpcProxy) isHandling(resp http.ResponseWriter,
	req *http.Request) bool {

	// gRPC web requests are easy to identify. Send them to the gRPC
	// web proxy.
	if p.grpcWebProxy.IsGrpcWebRequest(req) ||
		p.grpcWebProxy.IsGrpcWebSocketRequest(req) {

		log.Infof("Handling gRPC web request: %s", req.URL.Path)
		p.grpcWebProxy.ServeHTTP(resp, req)

		return true
	}

	// Normal gRPC requests are also easy to identify. These we can
	// send directly to the lnd proxy's gRPC server.
	if isGrpcRequest(req) {
		log.Infof("Handling gRPC request: %s", req.URL.Path)
		p.grpcServer.ServeHTTP(resp, req)

		return true
	}

	return false
}

// director is a function that directs an incoming request to the correct
// backend, depending on what kind of authentication information is attached to
// the request.
func (p *rpcProxy) director(ctx context.Context,
	requestURI string) (context.Context, *grpc.ClientConn, error) {

	// If this header is present in the request from the web client,
	// the actual connection to the backend will not be established.
	// https://github.com/improbable-eng/grpc-web/issues/568
	md, _ := metadata.FromIncomingContext(ctx)
	mdCopy := md.Copy()
	delete(mdCopy, "connection")

	outCtx := metadata.NewOutgoingContext(ctx, mdCopy)

	// Direct the call to the correct backend. All gRPC calls end up here
	// since our gRPC server instance doesn't have any handlers registered
	// itself. So all daemon calls that are remote are forwarded to them
	// directly. Everything else will go to lnd since it must either be an
	// lnd call or something that'll be handled by the integrated daemons
	// that are hooking into lnd's gRPC server.
	switch {
	case isFaradayURI(requestURI) && p.cfg.faradayRemote:
		return outCtx, p.faradayConn, nil

	case isLoopURI(requestURI) && p.cfg.loopRemote:
		return outCtx, p.loopConn, nil

	case isPoolURI(requestURI) && p.cfg.poolRemote:
		return outCtx, p.poolConn, nil

	default:
		return outCtx, p.lndConn, nil
	}
}

// UnaryServerInterceptor is a gRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (p *rpcProxy) UnaryServerInterceptor(
	permissionMap map[string][]bakery.Op) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		uriPermissions, ok := permissionMap[info.FullMethod]
		if !ok {
			return nil, fmt.Errorf("%s: unknown permissions "+
				"required for method", info.FullMethod)
		}

		var (
			err    error
			newCtx context.Context
		)
		// If we're in stateless remote mode, we already have the
		// macaroon to parse.
		if p.cfg.LndMode == ModeStatelessRemote {
			newCtx, err = p.getLndConnectStr(ctx)
			if err != nil {
				return nil, fmt.Errorf("error retrieving "+
					"macaroon from lndconnect string: %v",
					err)
			}
		} else {
			// For now, basic authentication is just a quick fix
			// until we have proper macaroon support implemented in
			// the UI. We allow gRPC web requests to have it and
			// "convert" the auth into a proper macaroon now.
			newCtx, err = p.basicAuthToMacaroon(
				ctx,
				info.FullMethod,
			)
			if err != nil {
				return nil, fmt.Errorf("error upgrading "+
					"basic auth: %v", err)
			}
		}

		// With the basic auth converted to a macaroon if necessary,
		// let's now validate the macaroon.
		err = p.macValidator.ValidateMacaroon(
			newCtx, uriPermissions, info.FullMethod,
		)
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (p *rpcProxy) StreamServerInterceptor(
	permissionMap map[string][]bakery.Op) grpc.StreamServerInterceptor {

	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		uriPermissions, ok := permissionMap[info.FullMethod]
		if !ok {
			return fmt.Errorf("%s: unknown permissions required "+
				"for method", info.FullMethod)
		}

		var (
			err    error
			newCtx context.Context
		)
		// If we're in stateless remote mode, we already have the
		// macaroon to parse.
		if p.cfg.LndMode == ModeStatelessRemote {
			newCtx, err = p.getLndConnectStr(ss.Context())
			if err != nil {
				return fmt.Errorf("error retrieving "+
					"data from lndconnect string: %v",
					err)
			}
		} else {
			// For now, basic authentication is just a quick fix
			// until we have proper macaroon support implemented in
			// the UI. We allow gRPC web requests to have it and
			// "convert" the auth into a proper macaroon now.
			newCtx, err = p.basicAuthToMacaroon(
				ss.Context(),
				info.FullMethod,
			)
			if err != nil {
				return fmt.Errorf("error upgrading "+
					"basic auth: %v", err)
			}
		}

		// With the basic auth converted to a macaroon if necessary,
		// let's now validate the macaroon.
		err = p.macValidator.ValidateMacaroon(
			newCtx, uriPermissions, info.FullMethod,
		)
		if err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

// basicAuthToMacaroon checks that the incoming request context has the expected
// and valid basic authentication header then attaches the correct macaroon to
// the context so it can be forwarded to the actual gRPC server.
func (p *rpcProxy) basicAuthToMacaroon(ctx context.Context,
	requestURI string) (context.Context, error) {

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, nil
	}

	authHeaders := md.Get("authorization")
	if len(authHeaders) == 0 {
		// No basic auth provided, we don't add a macaroon and let the
		// gRPC security interceptor reject the request.
		return ctx, nil
	}

	// The user specified an authorization header so this is very likely a
	// gRPC Web call from the UI. But we only attach the macaroon if the
	// auth is correct. That way an attacker doesn't know that basic auth
	// is even allowed as the error message will only be the macaroon error
	// from the lnd backend.
	authHeaderParts := strings.Split(authHeaders[0], " ")
	if len(authHeaderParts) != 2 {
		return ctx, nil
	}
	if authHeaderParts[1] != p.basicAuth {
		return ctx, nil
	}

	var macPath string
	switch {
	case isLndURI(requestURI):
		_, _, _, macPath = p.cfg.lndConnectParams()

	case isFaradayURI(requestURI):
		if p.cfg.faradayRemote {
			macPath = p.cfg.Remote.Faraday.MacaroonPath
		} else {
			macPath = p.cfg.Faraday.MacaroonPath
		}

	case isLoopURI(requestURI):
		if p.cfg.loopRemote {
			macPath = p.cfg.Remote.Loop.MacaroonPath
		} else {
			macPath = p.cfg.Loop.MacaroonPath
		}

	case isPoolURI(requestURI):
		if p.cfg.poolRemote {
			macPath = p.cfg.Remote.Pool.MacaroonPath
		} else {
			macPath = p.cfg.Pool.MacaroonPath
		}

	default:
		return ctx, fmt.Errorf("unknown gRPC web request: %v",
			requestURI)
	}

	// Now that we know which macaroon to load, do it and attach it to the
	// request context.
	macBytes, err := readMacaroon(lncfg.CleanAndExpandPath(macPath))
	if err != nil {
		return ctx, fmt.Errorf("error reading macaroon: %v", err)
	}
	md.Set(HeaderMacaroon, hex.EncodeToString(macBytes))
	return metadata.NewIncomingContext(ctx, md), nil
}

// getLndConnectStr is used in stateless remote mode to extract the data from
// the lndconnect string the user should have passed in.
func (p *rpcProxy) getLndConnectStr(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, nil
	}

	authHeaders := md.Get("authorization")
	if len(authHeaders) == 0 {
		// No lndconnect string provided, we let the gRPC security
		// interceptor reject the request.
		return ctx, nil
	}

	lndConnectParts := strings.Split(authHeaders[0], " ")
	lndConnectStr := lndConnectParts[1]

	// Since we're in stateless remote mode, we need to extract the
	// macaroon parameter from the lndconnect url that was passed in,
	// then attatch it to the request context.
	u, err := url.Parse(lndConnectStr)
	if err != nil {
		return ctx, nil
	}

	queryMap, _ := url.ParseQuery(u.RawQuery)
	macaroon := queryMap["macaroon"][0]

	// If channel isn't closed, it means we haven't sent the info from the
	// lndconnect string, and need to do so, so we can connect to
	// LND.
	if !p.lndConnectSent {
		tlsCert := queryMap["cert"][0]

		tlsCert = strings.Replace(tlsCert, " ", "+", 1)

		// Since the cert was passed in the lndConnect string without
		// linebreaks or a prefix/suffix, we need to turn the
		// certificate back into something the PEM library can parse.
		tlsCert = insertNth(tlsCert, 64, '\n')
		certPrefix := "-----BEGIN CERTIFICATE-----\n"
		certSuffix := "\n-----END CERTIFICATE-----"

		fullCert := certPrefix + tlsCert + certSuffix

		p.cfg.Remote.Lnd = &RemoteDaemonConfig{
			RPCServer: u.Host,
			Macaroon:  macaroon,
			TLSCert:   fullCert,
		}

		// We also need to establish an LND connection for the proxy.
		p.lndConn, err = dialBackend("lnd", u.Host, "", fullCert)
		if err != nil {
			log.Errorf("could not dial lnd: %v", err)

		}

		// Close the channel when we're done to signal to
		// startSubserver that we're ready to connect to LND.
		close(p.lndConnectChan)
		p.lndConnectSent = true
	}

	md.Set(HeaderMacaroon, macaroon)
	return metadata.NewIncomingContext(ctx, md), nil
}

func insertNth(s string, n int, insert rune) string {
	var buffer bytes.Buffer
	var n_1 = n - 1
	var l_1 = len(s) - 1

	for i, char := range s {
		buffer.WriteRune(char)
		if i%n == n_1 && i != l_1 {
			buffer.WriteRune(insert)
		}
	}

	return buffer.String()
}

// dialBackend connects to a gRPC backend through the given address and uses the
// given TLS certificate to authenticate the connection.
func dialBackend(name, dialAddr, tlsCertPath, tlsCertData string) (
	*grpc.ClientConn, error) {

	tlsConfig, err := lndclient.GetTLSCredentials(tlsCertData, tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("unable to get tls creds: %v", err)
	}

	var opts []grpc.DialOption

	opts = append(
		opts,

		// From the grpcProxy doc: This codec is *crucial* to the
		// functioning of the proxy.
		grpc.WithCodec(grpcProxy.Codec()), // nolint
		grpc.WithTransportCredentials(tlsConfig),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.DefaultConfig,
			MinConnectTimeout: defaultConnectTimeout,
		}),
	)

	log.Infof("Dialing %s gRPC server at %s", name, dialAddr)
	cc, err := grpc.Dial(dialAddr, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed dialing %s backend: %v", name,
			err)
	}
	return cc, nil
}

// readMacaroon tries to read the macaroon file at the specified path and create
// gRPC dial options from it.
func readMacaroon(macPath string) ([]byte, error) {
	// Load the specified macaroon file.
	macBytes, err := ioutil.ReadFile(macPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read macaroon path : %v", err)
	}

	// Make sure it actually is a macaroon by parsing it.
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %v", err)
	}

	// It's a macaroon alright, let's return the binary data now.
	return macBytes, nil
}

// isGrpcRequest determines if a request is a gRPC request by checking that the
// "content-type" is "application/grpc" and that the protocol is HTTP/2.
func isGrpcRequest(req *http.Request) bool {
	contentType := req.Header.Get("content-type")
	return req.ProtoMajor == 2 &&
		strings.HasPrefix(contentType, contentTypeGrpc)
}
