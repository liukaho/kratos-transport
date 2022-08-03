package nsq

import (
	"bytes"
	"context"
	"encoding/gob"
	"github.com/go-kratos/kratos/v2/log"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	NSQ "github.com/nsqio/go-nsq"
	"github.com/tx7do/kratos-transport/broker"
)

var (
	DefaultConcurrentHandlers = 1
)

const (
	defaultAddr = "127.0.0.1:4150"
)

type nsqBroker struct {
	lookupAddrs []string
	addrs       []string

	opts   broker.Options
	config *NSQ.Config

	sync.Mutex
	running bool

	log *log.Helper

	producers   []*NSQ.Producer
	subscribers []*subscriber
}

func NewBroker(opts ...broker.Option) broker.Broker {
	options := broker.NewOptionsAndApply(opts...)

	var addrs []string

	for _, addr := range options.Addrs {
		if len(addr) > 0 {
			addrs = append(addrs, addr)
		}
	}

	if len(addrs) == 0 {
		addrs = []string{defaultAddr}
	}

	n := &nsqBroker{
		addrs:  addrs,
		opts:   options,
		config: NSQ.NewConfig(),
		log:    log.NewHelper(log.GetLogger()),
	}

	n.configure(n.opts.Context)

	return n
}

func (b *nsqBroker) Name() string {
	return "nsq"
}

func (b *nsqBroker) Options() broker.Options {
	return b.opts
}

func (b *nsqBroker) Address() string {
	return b.addrs[rand.Intn(len(b.addrs))]
}

func (b *nsqBroker) Init(opts ...broker.Option) error {
	for _, o := range opts {
		o(&b.opts)
	}

	var addrs []string

	for _, addr := range b.opts.Addrs {
		if len(addr) > 0 {
			addrs = append(addrs, addr)
		}
	}

	if len(addrs) == 0 {
		addrs = []string{defaultAddr}
	}

	b.addrs = addrs
	b.configure(b.opts.Context)
	return nil
}

func (b *nsqBroker) configure(ctx context.Context) {
	if v, ok := ctx.Value(lookupdAddrsKey{}).([]string); ok {
		b.lookupAddrs = v
	}

	if v, ok := ctx.Value(consumerOptsKey{}).([]string); ok {
		cfgFlag := &NSQ.ConfigFlag{Config: b.config}
		for _, opt := range v {
			_ = cfgFlag.Set(opt)
		}
	}
}

func (b *nsqBroker) Connect() error {
	b.Lock()
	defer b.Unlock()

	if b.running {
		return nil
	}

	producers := make([]*NSQ.Producer, 0, len(b.addrs))

	for _, addr := range b.addrs {
		p, err := NSQ.NewProducer(addr, b.config)
		if err != nil {
			return err
		}
		if err = p.Ping(); err != nil {
			return err
		}
		producers = append(producers, p)
	}

	for _, c := range b.subscribers {
		channel := c.opts.Queue
		if len(channel) == 0 {
			channel = uuid.New().String() + "#ephemeral"
		}

		cm, err := NSQ.NewConsumer(c.topic, channel, b.config)
		if err != nil {
			return err
		}

		if c.handlerFunc != nil {
			cm.AddConcurrentHandlers(c.handlerFunc, c.concurrency)
		}

		c.consumer = cm

		if len(b.lookupAddrs) > 0 {
			_ = c.consumer.ConnectToNSQLookupds(b.lookupAddrs)
		} else {
			err = c.consumer.ConnectToNSQDs(b.addrs)
			if err != nil {
				return err
			}
		}
	}

	b.producers = producers
	b.running = true
	return nil
}

func (b *nsqBroker) Disconnect() error {
	b.Lock()
	defer b.Unlock()

	if !b.running {
		return nil
	}

	for _, p := range b.producers {
		p.Stop()
	}

	for _, c := range b.subscribers {
		c.consumer.Stop()

		if len(b.lookupAddrs) > 0 {
			for _, addr := range b.lookupAddrs {
				_ = c.consumer.DisconnectFromNSQLookupd(addr)
			}
		} else {
			for _, addr := range b.addrs {
				_ = c.consumer.DisconnectFromNSQD(addr)
			}
		}
	}

	b.producers = nil
	b.running = false
	return nil
}

func (b *nsqBroker) Publish(topic string, msg broker.Any, opts ...broker.PublishOption) error {
	if b.opts.Codec != nil {
		var err error
		buf, err := b.opts.Codec.Marshal(msg)
		if err != nil {
			return err
		}
		return b.publish(topic, buf, opts...)
	} else {
		switch t := msg.(type) {
		case []byte:
			return b.publish(topic, t, opts...)
		case string:
			return b.publish(topic, []byte(t), opts...)
		default:
			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			if err := enc.Encode(msg); err != nil {
				return err
			}
			return b.publish(topic, buf.Bytes(), opts...)
		}
	}
}

func (b *nsqBroker) publish(topic string, msg []byte, opts ...broker.PublishOption) error {
	options := broker.PublishOptions{}
	for _, o := range opts {
		o(&options)
	}

	var (
		doneChan chan *NSQ.ProducerTransaction
		delay    time.Duration
	)
	if options.Context != nil {
		if v, ok := options.Context.Value(asyncPublishKey{}).(chan *NSQ.ProducerTransaction); ok {
			doneChan = v
		}
		if v, ok := options.Context.Value(deferredPublishKey{}).(time.Duration); ok {
			delay = v
		}
	}

	p := b.producers[rand.Intn(len(b.producers))]

	if doneChan != nil {
		if delay > 0 {
			return p.DeferredPublishAsync(topic, delay, msg, doneChan)
		}
		return p.PublishAsync(topic, msg, doneChan)
	} else {
		if delay > 0 {
			return p.DeferredPublish(topic, delay, msg)
		}
		return p.Publish(topic, msg)
	}
}

func (b *nsqBroker) Subscribe(topic string, handler broker.Handler, binder broker.Binder, opts ...broker.SubscribeOption) (broker.Subscriber, error) {
	options := broker.SubscribeOptions{
		AutoAck: true,
	}

	for _, o := range opts {
		o(&options)
	}

	concurrency, maxInFlight := DefaultConcurrentHandlers, DefaultConcurrentHandlers
	if options.Context != nil {
		if v, ok := options.Context.Value(concurrentHandlerKey{}).(int); ok {
			maxInFlight, concurrency = v, v
		}
		if v, ok := options.Context.Value(maxInFlightKey{}).(int); ok {
			maxInFlight = v
		}
	}

	config := *b.config
	config.MaxInFlight = maxInFlight

	channel := options.Queue
	if len(channel) == 0 {
		channel = uuid.New().String() + "#ephemeral"
	}

	c, err := NSQ.NewConsumer(topic, channel, &config)
	if err != nil {
		return nil, err
	}

	h := NSQ.HandlerFunc(func(nm *NSQ.Message) error {
		if !options.AutoAck {
			nm.DisableAutoResponse()
		}

		//fmt.Println("receive message:", nm.ID, nm.Body)

		var m broker.Message

		if binder != nil {
			m.Body = binder()
		}

		if b.opts.Codec != nil {
			if err := b.opts.Codec.Unmarshal(nm.Body, m.Body); err != nil {
				return err
			}
		} else {
			m.Body = nm.Body
		}

		p := &publication{topic: topic, nsqMsg: nm, msg: &m}

		if err := handler(b.opts.Context, p); err != nil {
			p.err = err
		}

		if options.AutoAck {
			if err := p.Ack(); err != nil {
				b.log.Errorf("[nats]: unable to commit msg: %v", err)
			}
		}

		return p.err
	})

	c.AddConcurrentHandlers(h, concurrency)

	if len(b.lookupAddrs) > 0 {
		err = c.ConnectToNSQLookupds(b.lookupAddrs)
	} else {
		err = c.ConnectToNSQDs(b.addrs)
	}
	if err != nil {
		return nil, err
	}

	sub := &subscriber{
		consumer:    c,
		opts:        options,
		topic:       topic,
		handlerFunc: h,
		concurrency: concurrency,
	}

	b.subscribers = append(b.subscribers, sub)

	return sub, nil
}
