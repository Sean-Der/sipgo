package sipgo

import (
	"context"
	"net"
	"strings"

	"github.com/emiraganov/sipgo/sip"
	"github.com/emiraganov/sipgo/transaction"
	"github.com/emiraganov/sipgo/transport"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// RequestHandler is a callback that will be called on the incoming request
type RequestHandler func(req *sip.Request, tx sip.ServerTransaction)

// Server is a SIP server
type Server struct {
	tp          *transport.Layer
	tx          *transaction.Layer
	ip          net.IP
	host        string
	port        int
	dnsResolver *net.Resolver
	userAgent   string

	// requestHandlers map of all registered request handlers
	requestHandlers map[sip.RequestMethod]RequestHandler
	listeners       map[string]string //addr:network

	//Serve request is middleware run before any request received
	serveMessage func(m sip.Message)

	log zerolog.Logger

	requestCallback  func(r *sip.Request)
	responseCallback func(r *sip.Response)

	// Default server behavior for sending request in preflight
	AddViaHeader   bool
	AddRecordRoute bool
}

type ServerOption func(s *Server) error

func WithLogger(logger zerolog.Logger) ServerOption {
	return func(s *Server) error {
		s.log = logger
		return nil
	}
}

func WithIP(ip string) ServerOption {
	return func(s *Server) error {
		host, _, err := net.SplitHostPort(ip)
		if err != nil {
			return err
		}
		addr, err := net.ResolveIPAddr("ip", host)
		if err != nil {
			return err
		}
		return s.setIP(addr.IP)
	}
}

func WithDNSResolver(r *net.Resolver) ServerOption {
	return func(s *Server) error {
		s.dnsResolver = r
		return nil
	}
}

func WithUDPDNSResolver(dns string) ServerOption {
	return func(s *Server) error {
		s.dnsResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{}
				return d.DialContext(ctx, "udp", dns)
			},
		}
		return nil
	}
}

func WithUserAgent(ua string) ServerOption {
	return func(s *Server) error {
		s.userAgent = ua
		return nil
	}
}

// NewServer creates new instance of SIP server.
func NewServer(options ...ServerOption) (*Server, error) {
	s := &Server{
		userAgent:       "SIPGO",
		dnsResolver:     net.DefaultResolver,
		requestHandlers: make(map[sip.RequestMethod]RequestHandler),
		listeners:       make(map[string]string),
		log:             log.Logger.With().Str("caller", "Server").Logger(),
		AddViaHeader:    true,
		AddRecordRoute:  true,
	}
	for _, o := range options {
		if err := o(s); err != nil {
			return nil, err
		}
	}

	if s.ip == nil {
		v, err := sip.ResolveSelfIP()
		if err != nil {
			return nil, err
		}
		if err := s.setIP(v); err != nil {
			return nil, err
		}
	}

	s.tp = transport.NewLayer(s.dnsResolver)
	s.tx = transaction.NewLayer(s.tp, s.onRequest)

	return s, nil
}

// Listen adds listener for serve
func (srv *Server) setIP(ip net.IP) (err error) {
	srv.ip = ip
	srv.host = strings.Split(ip.String(), ":")[0]
	return err
}

// Listen adds listener for serve
func (srv *Server) Listen(network string, addr string) {
	srv.listeners[addr] = network
}

// Serve will fire all listeners
func (srv *Server) Serve() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return srv.ServeWithContext(ctx)
}

// Serve will fire all listeners. Ctx allows canceling
func (srv *Server) ServeWithContext(ctx context.Context) error {
	defer srv.shutdown()
	for addr, network := range srv.listeners {
		go srv.tp.Serve(ctx, network, addr)
	}
	<-ctx.Done()
	return ctx.Err()
}

// onRequest gets request from Transaction layer
func (srv *Server) onRequest(req *sip.Request, tx sip.ServerTransaction) {
	go srv.handleRequest(req, tx)
}

// handleRequest must be run in seperate goroutine
func (srv *Server) handleRequest(req *sip.Request, tx sip.ServerTransaction) {
	if srv.requestCallback != nil {
		srv.requestCallback(req)
	}

	handler := srv.getHandler(req.Method())

	if handler == nil {
		srv.log.Warn().Msg("SIP request handler not found")
		res := sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil)
		if err := srv.WriteResponse(res); err != nil {
			srv.log.Error().Msgf("respond '405 Method Not Allowed' failed: %s", err)
		}

		for {
			select {
			case <-tx.Done():
				return
			case err, ok := <-tx.Errors():
				if !ok {
					return
				}
				srv.log.Warn().Msgf("error from SIP server transaction %s: %s", tx, err)
			}
		}
	}

	handler(req, tx)
	if tx != nil {
		// Must be called to prevent any transaction leaks
		tx.Terminate()
	}
}

// TransactionRequest sends sip request and initializes client transaction
// It prepends Via header by default
func (srv *Server) TransactionRequest(req *sip.Request) (sip.ClientTransaction, error) {
	/*
		To consider
		18.2.1 We could have to change network if message is to large for UDP
	*/
	srv.updateRequest(req)
	return srv.tx.Request(req)
}

// TransactionReply is wrapper for calling tx.Respond
// it handles removing Via header by default
func (srv *Server) TransactionReply(tx sip.ServerTransaction, res *sip.Response) error {
	srv.updateResponse(res)
	return tx.Respond(res)
}

// WriteRequest will proxy message to transport layer. Use it in stateless mode
func (srv *Server) WriteRequest(r *sip.Request) error {
	srv.updateRequest(r)
	return srv.tp.WriteMsg(r)
}

// WriteResponse will proxy message to transport layer. Use it in stateless mode
func (srv *Server) WriteResponse(r *sip.Response) error {
	return srv.tp.WriteMsg(r)
}

func (srv *Server) updateRequest(r *sip.Request) {
	// We handle here only INVITE and BYE
	// https://www.rfc-editor.org/rfc/rfc3261.html#section-16.6
	if srv.AddViaHeader {
		if via, exists := r.Via(); exists {
			newvia := via.Clone()
			newvia.Host = srv.host
			newvia.Port = srv.port
			r.PrependHeader(newvia)

			if via.Params.Has("rport") {
				h, p, _ := net.SplitHostPort(r.Source())
				via.Params.Add("rport", p)
				via.Params.Add("received", h)
			}
		}
	}

	if srv.AddRecordRoute {
		rr := &sip.RecordRouteHeader{
			Address: sip.Uri{
				Host: srv.host,
				Port: srv.port,
				UriParams: sip.HeaderParams{
					// Transport must be provided as well
					// https://datatracker.ietf.org/doc/html/rfc5658
					"transport": transport.NetworkToLower(r.Transport()),
					"lr":        "",
				},
			},
		}

		r.PrependHeader(rr)
	}

}

func (srv *Server) updateResponse(r *sip.Response) {
	if srv.AddViaHeader {
		srv.RemoveVia(r)
	}
}

// RemoveVia can be used in case of sending response.
func (srv *Server) RemoveVia(r *sip.Response) {
	if via, exists := r.Via(); exists {
		if via.Host == srv.host {
			// In case it is multipart Via remove only one
			if via.Next != nil {
				via.Remove()
			} else {
				r.RemoveHeader("Via")
			}
		}
	}
}

// Shutdown gracefully shutdowns SIP server
func (srv *Server) shutdown() {
	// stop transaction layer
	srv.tx.Close()
	// stop transport layer
	srv.tp.Close()
}

// OnRequest registers new request callback. Can be used as generic way to add handler
func (srv *Server) OnRequest(method sip.RequestMethod, handler RequestHandler) {
	srv.requestHandlers[method] = handler
}

// OnInvite registers Invite request handler
func (srv *Server) OnInvite(handler RequestHandler) {
	srv.requestHandlers[sip.INVITE] = handler
}

// OnAck registers Ack request handler
func (srv *Server) OnAck(handler RequestHandler) {
	srv.requestHandlers[sip.ACK] = handler
}

// OnCancel registers Cancel request handler
func (srv *Server) OnCancel(handler RequestHandler) {
	srv.requestHandlers[sip.CANCEL] = handler
}

// OnBye registers Bye request handler
func (srv *Server) OnBye(handler RequestHandler) {
	srv.requestHandlers[sip.BYE] = handler
}

// OnRegister registers Register request handler
func (srv *Server) OnRegister(handler RequestHandler) {
	srv.requestHandlers[sip.REGISTER] = handler
}

// OnOptions registers Options request handler
func (srv *Server) OnOptions(handler RequestHandler) {
	srv.requestHandlers[sip.OPTIONS] = handler
}

// OnSubscribe registers Subscribe request handler
func (srv *Server) OnSubscribe(handler RequestHandler) {
	srv.requestHandlers[sip.SUBSCRIBE] = handler
}

// OnNotify registers Notify request handler
func (srv *Server) OnNotify(handler RequestHandler) {
	srv.requestHandlers[sip.NOTIFY] = handler
}

// OnRefer registers Refer request handler
func (srv *Server) OnRefer(handler RequestHandler) {
	srv.requestHandlers[sip.REFER] = handler
}

// OnInfo registers Info request handler
func (srv *Server) OnInfo(handler RequestHandler) {
	srv.requestHandlers[sip.INFO] = handler
}

// OnMessage registers Message request handler
func (srv *Server) OnMessage(handler RequestHandler) {
	srv.requestHandlers[sip.MESSAGE] = handler
}

// OnPrack registers Prack request handler
func (srv *Server) OnPrack(handler RequestHandler) {
	srv.requestHandlers[sip.PRACK] = handler
}

// OnUpdate registers Update request handler
func (srv *Server) OnUpdate(handler RequestHandler) {
	srv.requestHandlers[sip.UPDATE] = handler
}

// OnPublish registers Publish request handler
func (srv *Server) OnPublish(handler RequestHandler) {
	srv.requestHandlers[sip.PUBLISH] = handler
}

func (srv *Server) getHandler(method sip.RequestMethod) (handler RequestHandler) {
	handler, ok := srv.requestHandlers[method]
	if !ok {
		return nil
	}
	return handler
}

// ServeRequest can be used as middleware for preprocessing message
// It process all received requests and all received responses.
// NOTE: It can only be called once
func (srv *Server) ServeRequest(f func(r *sip.Request)) {
	if srv.requestCallback != nil {
		panic("request callback can only be assigned once")
	}
	srv.requestCallback = f
}

// TODO can this handled better?
func (srv *Server) ServeResponse(f func(m *sip.Response)) {
	if srv.responseCallback != nil {
		panic("response callback can only be assigned once")
	}
	srv.responseCallback = f
	srv.tp.OnMessage(srv.onTransportMessage)
}

func (srv *Server) onTransportMessage(m sip.Message) {
	//Register transport middleware
	// this avoids allocations and it forces devs to avoid sip.Message usage
	switch r := m.(type) {
	case *sip.Response:
		srv.responseCallback(r)
	}
}

// Transport is function to get transport layer of server
// Can be used for modifying
func (srv *Server) TransportLayer() *transport.Layer {
	return srv.tp
}
