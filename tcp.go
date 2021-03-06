// Package tcp implements a go-micro.Server
package tcp

import (
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/unistack-org/micro/v3/broker"
	"github.com/unistack-org/micro/v3/codec"
	"github.com/unistack-org/micro/v3/logger"
	"github.com/unistack-org/micro/v3/register"
	"github.com/unistack-org/micro/v3/server"
	"golang.org/x/net/netutil"
)

type tcpServer struct {
	sync.RWMutex
	opts         server.Options
	hd           server.Handler
	exit         chan chan error
	registerOnce sync.Once
	subscribers  map[*tcpSubscriber][]broker.Subscriber
	// used for first registration
	registered bool
	// register service instance
	rsvc *register.Service
}

func (h *tcpServer) newCodec(ct string) (codec.Codec, error) {
	if cf, ok := h.opts.Codecs[ct]; ok {
		return cf, nil
	}
	return nil, codec.ErrUnknownContentType
}

func (h *tcpServer) Options() server.Options {
	h.RLock()
	defer h.RUnlock()
	return h.opts
}

func (h *tcpServer) Init(opts ...server.Option) error {
	h.Lock()
	for _, o := range opts {
		o(&h.opts)
	}
	h.Unlock()
	return nil
}

func (h *tcpServer) Handle(handler server.Handler) error {
	h.Lock()
	h.hd = handler
	h.Unlock()
	return nil
}

func (h *tcpServer) NewHandler(handler interface{}, opts ...server.HandlerOption) server.Handler {
	options := server.NewHandlerOptions(opts...)

	var eps []*register.Endpoint

	if !options.Internal {
		for name, metadata := range options.Metadata {
			eps = append(eps, &register.Endpoint{
				Name:     name,
				Metadata: metadata,
			})
		}
	}

	th := &tcpHandler{
		eps:  eps,
		hd:   handler,
		opts: options,
	}

	if size, ok := h.opts.Context.Value(maxMsgSizeKey{}).(int); ok && size > 0 {
		th.maxMsgSize = size
	}

	return th
}

func (h *tcpServer) NewSubscriber(topic string, handler interface{}, opts ...server.SubscriberOption) server.Subscriber {
	return newSubscriber(topic, handler, opts...)
}

func (h *tcpServer) Subscribe(sb server.Subscriber) error {
	sub, ok := sb.(*tcpSubscriber)
	if !ok {
		return fmt.Errorf("invalid subscriber: expected *tcpSubscriber")
	}
	if len(sub.handlers) == 0 {
		return fmt.Errorf("invalid subscriber: no handler functions")
	}

	if err := validateSubscriber(sb); err != nil {
		return err
	}

	h.Lock()
	defer h.Unlock()
	_, ok = h.subscribers[sub]
	if ok {
		return fmt.Errorf("subscriber %v already exists", h)
	}
	h.subscribers[sub] = nil
	return nil
}

func (h *tcpServer) Register() error {
	h.Lock()
	config := h.opts
	rsvc := h.rsvc
	eps := h.hd.Endpoints()
	h.Unlock()

	// if service already filled, reuse it and return early
	if rsvc != nil {
		if err := server.DefaultRegisterFunc(rsvc, config); err != nil {
			return err
		}
		return nil
	}

	service, err := server.NewRegisterService(h)
	if err != nil {
		return err
	}
	service.Nodes[0].Metadata["protocol"] = "tcp"
	service.Nodes[0].Metadata["transport"] = "tcp"
	service.Endpoints = eps

	h.Lock()
	var subscriberList []*tcpSubscriber
	for e := range h.subscribers {
		// Only advertise non internal subscribers
		if !e.Options().Internal {
			subscriberList = append(subscriberList, e)
		}
	}
	sort.Slice(subscriberList, func(i, j int) bool {
		return subscriberList[i].topic > subscriberList[j].topic
	})
	for _, e := range subscriberList {
		service.Endpoints = append(service.Endpoints, e.Endpoints()...)
	}
	h.Unlock()

	h.RLock()
	registered := h.registered
	h.RUnlock()

	if !registered {
		if config.Logger.V(logger.InfoLevel) {
			config.Logger.Infof(config.Context, "Register [%s] Registering node: %s", config.Register.String(), service.Nodes[0].Id)
		}
	}

	// register the service
	if err := server.DefaultRegisterFunc(service, config); err != nil {
		return err
	}

	// already registered? don't need to register subscribers
	if registered {
		return nil
	}

	h.Lock()
	defer h.Unlock()

	if h.registered {
		return nil
	}

	for sb := range h.subscribers {
		handler := h.createSubHandler(sb, config)
		var opts []broker.SubscribeOption
		if queue := sb.Options().Queue; len(queue) > 0 {
			opts = append(opts, broker.SubscribeGroup(queue))
		}

		subCtx := config.Context
		if cx := sb.Options().Context; cx != nil {
			subCtx = cx
		}
		opts = append(opts, broker.SubscribeContext(subCtx))
		opts = append(opts, broker.SubscribeAutoAck(sb.Options().AutoAck))

		if config.Logger.V(logger.InfoLevel) {
			config.Logger.Infof(config.Context, "Subscribing to topic: %s", sb.Topic())
		}

		sub, err := config.Broker.Subscribe(subCtx, sb.Topic(), handler, opts...)
		if err != nil {
			return err
		}
		h.subscribers[sb] = []broker.Subscriber{sub}
	}

	h.registered = true
	h.rsvc = service

	return nil
}

func (h *tcpServer) Deregister() error {
	h.Lock()
	config := h.opts
	h.Unlock()

	service, err := server.NewRegisterService(h)
	if err != nil {
		return err
	}

	if config.Logger.V(logger.InfoLevel) {
		config.Logger.Infof(config.Context, "Deregistering node: %s", service.Nodes[0].Id)
	}

	if err := server.DefaultDeregisterFunc(service, config); err != nil {
		return err
	}

	h.Lock()
	if !h.registered {
		h.Unlock()
		return nil
	}
	h.registered = false

	wg := sync.WaitGroup{}
	subCtx := h.opts.Context

	for sb, subs := range h.subscribers {
		if cx := sb.Options().Context; cx != nil {
			subCtx = cx
		}

		for _, sub := range subs {
			wg.Add(1)
			go func(s broker.Subscriber) {
				defer wg.Done()
				if config.Logger.V(logger.InfoLevel) {
					config.Logger.Infof(config.Context, "Unsubscribing from topic: %s", s.Topic())
				}
				if err := s.Unsubscribe(subCtx); err != nil {
					if config.Logger.V(logger.ErrorLevel) {
						config.Logger.Errorf(config.Context, "Unsubscribing from topic: %s err: %v", s.Topic(), err)
					}
				}
			}(sub)
		}
		h.subscribers[sb] = nil
	}
	wg.Wait()

	h.Unlock()
	return nil
}

func (h *tcpServer) getListener() net.Listener {
	if h.opts.Context == nil {
		return nil
	}

	l, ok := h.opts.Context.Value(netListener{}).(net.Listener)
	if !ok || l == nil {
		return nil
	}

	return l
}

func (h *tcpServer) Start() error {
	h.RLock()
	config := h.opts
	hd := h.hd.Handler()
	h.RUnlock()

	var err error
	var ts net.Listener

	if l := h.getListener(); l != nil {
		ts = l
	} else {
		// check the tls config for secure connect
		if tc := config.TLSConfig; tc != nil {
			ts, err = tls.Listen("tcp", config.Address, tc)
			// otherwise just plain tcp listener
		} else {
			ts, err = net.Listen("tcp", config.Address)
		}
		if err != nil {
			return err
		}

		if config.Context != nil {
			if c, ok := config.Context.Value(maxConnKey{}).(int); ok && c > 0 {
				ts = netutil.LimitListener(ts, c)
			}
		}
	}

	if config.Logger.V(logger.ErrorLevel) {
		config.Logger.Infof(config.Context, "Listening on %s", ts.Addr().String())
	}

	h.Lock()
	h.opts.Address = ts.Addr().String()
	h.Unlock()

	if err = config.Broker.Connect(config.Context); err != nil {
		return err
	}

	// register
	if err = h.Register(); err != nil {
		return err
	}

	handle, ok := hd.(Handler)
	if !ok {
		return fmt.Errorf("invalid handler %T", hd)
	}
	go h.serve(ts, handle)

	go func() {
		t := new(time.Ticker)

		// only process if it exists
		if config.RegisterInterval > time.Duration(0) {
			// new ticker
			t = time.NewTicker(config.RegisterInterval)
		}

		// return error chan
		var ch chan error

	Loop:
		for {
			select {
			// register self on interval
			case <-t.C:
				h.RLock()
				registered := h.registered
				h.RUnlock()
				rerr := h.opts.RegisterCheck(h.opts.Context)
				if rerr != nil && registered {
					if config.Logger.V(logger.ErrorLevel) {
						config.Logger.Errorf(config.Context, "Server %s-%s register check error: %s, deregister it", config.Name, config.Id, rerr)
					}
					// deregister self in case of error
					if err := h.Deregister(); err != nil {
						if config.Logger.V(logger.ErrorLevel) {
							config.Logger.Errorf(config.Context, "Server %s-%s deregister error: %s", config.Name, config.Id, err)
						}
					}
				} else if rerr != nil && !registered {
					if config.Logger.V(logger.ErrorLevel) {
						config.Logger.Errorf(config.Context, "Server %s-%s register check error: %s", config.Name, config.Id, rerr)
					}
					continue
				}
				if err := h.Register(); err != nil {
					if config.Logger.V(logger.ErrorLevel) {
						config.Logger.Errorf(config.Context, "Server %s-%s register error: %s", config.Name, config.Id, err)
					}
				}
				// wait for exit
			case ch = <-h.exit:
				break Loop
			}
		}

		ch <- ts.Close()

		// deregister
		h.Deregister()

		config.Broker.Disconnect(config.Context)
	}()

	return nil
}

func (h *tcpServer) Stop() error {
	ch := make(chan error)
	h.exit <- ch
	return <-ch
}

func (s *tcpServer) String() string {
	return "tcp"
}

func (s *tcpServer) Name() string {
	return s.opts.Name
}

func (s *tcpServer) serve(ln net.Listener, h Handler) {
	var tempDelay time.Duration // how long to sleep on accept failure
	s.RLock()
	config := s.opts
	s.RUnlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.exit:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				if config.Logger.V(logger.ErrorLevel) {
					config.Logger.Errorf(config.Context, "tcp: Accept error: %v; retrying in %v", err, tempDelay)
				}
				time.Sleep(tempDelay)
				continue
			}
			if config.Logger.V(logger.ErrorLevel) {
				config.Logger.Error(config.Context, "tcp: Accept error: %v", err)
			}
			return
		}

		if err != nil {
			config.Logger.Error(config.Context, "tcp: accept err: %v", err)
			return
		}
		go h.Serve(c)
	}
}

func NewServer(opts ...server.Option) server.Server {
	return &tcpServer{
		opts:        server.NewOptions(opts...),
		exit:        make(chan chan error),
		subscribers: make(map[*tcpSubscriber][]broker.Subscriber),
	}
}
