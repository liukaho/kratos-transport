package pulsar

import (
	"errors"
	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
	"github.com/tx7do/kratos-transport/broker"
	"sync"
	"time"
)

const (
	defaultAddr = "pulsar://127.0.0.1:6650"
)

type pulsarBroker struct {
	addrs []string

	log *log.Helper

	connected bool
	sync.RWMutex
	opts broker.Options

	client    pulsar.Client
	producers map[string]pulsar.Producer
}

func NewBroker(opts ...broker.Option) broker.Broker {
	options := broker.NewOptionsAndApply(opts...)

	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL:               defaultAddr,
		OperationTimeout:  30 * time.Second,
		ConnectionTimeout: 30 * time.Second,
	})
	if err != nil {
		log.Fatalf("Could not instantiate Pulsar client: %v", err)
	}

	r := &pulsarBroker{
		producers: make(map[string]pulsar.Producer),
		addrs:     options.Addrs,
		opts:      options,
		log:       log.NewHelper(log.GetLogger()),
		client:    client,
	}

	return r
}

func (pb *pulsarBroker) Name() string {
	return "pulsar"
}

func (pb *pulsarBroker) Address() string {
	if len(pb.addrs) > 0 {
		return pb.addrs[0]
	}
	return defaultAddr
}

func (pb *pulsarBroker) Options() broker.Options {
	return pb.opts
}

func (pb *pulsarBroker) Init(opts ...broker.Option) error {
	pb.opts.Apply(opts...)

	var cAddrs []string
	for _, addr := range pb.opts.Addrs {
		if len(addr) == 0 {
			continue
		}
		cAddrs = append(cAddrs, addr)
	}
	if len(cAddrs) == 0 {
		cAddrs = []string{defaultAddr}
	}
	pb.addrs = cAddrs

	return nil
}

func (pb *pulsarBroker) Connect() error {
	pb.RLock()
	if pb.connected {
		pb.RUnlock()
		return nil
	}
	pb.RUnlock()

	pb.Lock()
	pb.addrs = pb.opts.Addrs
	pb.connected = true
	pb.Unlock()

	return nil
}

func (pb *pulsarBroker) Disconnect() error {
	pb.RLock()
	if !pb.connected {
		pb.RUnlock()
		return nil
	}
	pb.RUnlock()

	pb.Lock()
	defer pb.Unlock()

	for _, p := range pb.producers {
		p.Close()
	}

	pb.client.Close()

	pb.connected = false
	return nil
}

func (pb *pulsarBroker) Publish(topic string, msg *broker.Message, opts ...broker.PublishOption) error {
	var cached bool

	options := pulsar.ProducerOptions{
		Topic:           topic,
		Name:            "my-producer",
		DisableBatching: true,
	}

	pb.Lock()
	producer, ok := pb.producers[topic]
	if !ok {
		var err error
		producer, err = pb.client.CreateProducer(options)
		if err != nil {
			pb.Unlock()
			return err
		}

		pb.producers[topic] = producer
	} else {
		cached = true
	}
	pb.Unlock()

	var buf []byte
	if pb.opts.Codec != nil {
		var err error
		buf, err = pb.opts.Codec.Marshal(msg)
		if err != nil {
			return err
		}
	} else {
		buf = msg.Body
	}

	rMsg := pulsar.ProducerMessage{Payload: buf}
	_, err := producer.Send(pb.opts.Context, &rMsg)
	if err != nil {
		pb.log.Errorf("[pulsar]: send message error: %s\n", err)
		switch cached {
		case false:
		case true:
			pb.Lock()
			producer.Close()
			delete(pb.producers, topic)
			pb.Unlock()

			producer, err = pb.client.CreateProducer(options)
			if err != nil {
				pb.Unlock()
				return err
			}
			if _, err = producer.Send(pb.opts.Context, &rMsg); err == nil {
				pb.Lock()
				pb.producers[topic] = producer
				pb.Unlock()
			}
		}
	}

	err = producer.Flush()
	if err != nil {
		return err
	}

	return nil
}

func (pb *pulsarBroker) Subscribe(topic string, h broker.Handler, opts ...broker.SubscribeOption) (broker.Subscriber, error) {
	opt := broker.SubscribeOptions{
		AutoAck: true,
		Queue:   uuid.New().String(),
	}
	for _, o := range opts {
		o(&opt)
	}

	options := pulsar.ConsumerOptions{
		Topic:            topic,
		SubscriptionName: "my-subscription",
		Type:             pulsar.Shared,
	}

	channel := make(chan pulsar.ConsumerMessage, 100)
	options.MessageChannel = channel

	c, _ := pb.client.Subscribe(options)
	if c == nil {
		return nil, errors.New("create consumer error")
	}

	sub := &subscriber{
		opts:    opt,
		topic:   topic,
		handler: h,
		reader:  c,
		channel: channel,
	}

	go func() {
		var err error
		var m broker.Message
		for cm := range channel {
			p := &publication{topic: cm.Topic(), reader: sub.reader, msg: &m, pulsarMsg: &cm.Message, ctx: opt.Context}
			m.Header = cm.Properties()
			if pb.opts.Codec != nil {
				if err := pb.opts.Codec.Unmarshal(cm.Payload(), &m); err != nil {
					p.err = err
				}
			} else {
				m.Body = cm.Payload()
			}

			err = sub.handler(sub.opts.Context, p)
			if err != nil {
				pb.log.Errorf("[pulsar]: process message failed: %v", err)
			}
			if sub.opts.AutoAck {
				if err = p.Ack(); err != nil {
					pb.log.Errorf("[pulsar]: unable to commit msg: %v", err)
				}
			}
		}
	}()

	return sub, nil
}
