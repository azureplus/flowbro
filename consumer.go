package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"golang.org/x/net/websocket"
)

func gowg(f func(), wg *sync.WaitGroup) {
	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		f()
		defer wg.Done()
	}(wg)
}

type cluster struct {
	brokers            []string
	consumer           sarama.Consumer
	client             sarama.Client
	partitionConsumers []sarama.PartitionConsumer
	pcL                sync.Mutex
}

func (c *cluster) close() {
	log.Printf("Trying to close cluster with brokers %v", c.brokers)

	log.Printf("Trying to close %v partition consumers for cluster with brokers %v", len(c.partitionConsumers), c.brokers)
	for _, pc := range c.partitionConsumers {
		if err := pc.Close(); err != nil {
			log.Printf("Error while trying to close partition consumer for cluster with brokers %v. err=%v", c.brokers, err)
		}
	}

	log.Printf("Trying to close consumer for cluster with brokers %v", c.brokers)
	if err := c.consumer.Close(); err != nil {
		log.Printf("Error while trying to close consumer for cluster with brokers %v. err=%v", c.brokers, err)
	} else {
		log.Printf("Successfully closed consumer for cluster with brokers %v", c.brokers)
	}

	log.Printf("Trying to close client for cluster with brokers %v", c.brokers)
	if err := c.client.Close(); err != nil {
		log.Printf("Error while trying to close client for cluster with brokers %v. err=%v", c.brokers, err)
	} else {
		log.Printf("Successfully closed client for cluster with brokers %v", c.brokers)
	}

	log.Printf("Finished trying to close cluster with brokers %v", c.brokers)
}

func closeAll(clusters map[string]*cluster) {
	log.Printf("Trying to close all clusters")
	for _, c := range clusters {
		c.close()
	}
	log.Printf("Finished trying to close all clusters")
}

type iKafkaUtils interface {
	newClient(brokers []string) (sarama.Client, error)
	newConsumerFromClient(client sarama.Client) (sarama.Consumer, error)
}

type kafkaUtils struct{}

func (k kafkaUtils) newClient(brokers []string) (sarama.Client, error) {
	return sarama.NewClient(brokers, nil)
}

func (k kafkaUtils) newConsumerFromClient(client sarama.Client) (sarama.Consumer, error) {
	return sarama.NewConsumerFromClient(client)
}

func setupClusters(clusters map[string]*cluster, utils iKafkaUtils) []error {
	errors := []error{}
	var errL sync.Mutex

	var wg sync.WaitGroup

	for b := range clusters {
		gowg(func(b string) func() {
			return func() {
				c := clusters[b]
				log.Printf("Adding client+consumer for cluster with brokers %v", c.brokers)
				client, err := utils.newClient(c.brokers)
				if err != nil {
					errL.Lock()
					errors = append(errors, fmt.Errorf("Error creating client. err=%v", err))
					errL.Unlock()
				}
				consumer, err := utils.newConsumerFromClient(client)
				if err != nil {
					errL.Lock()
					errors = append(errors, fmt.Errorf("Error creating consumer. err=%v", err))
					errL.Unlock()
				}
				c.client = client
				c.consumer = consumer
			}
		}(b), &wg)
	}
	wg.Wait()

	return errors
}

func setupPartitionConsumers(conf *Config) ([]<-chan *sarama.ConsumerMessage, map[string]*cluster, bool) {
	clusters := make(map[string]*cluster)
	for _, c := range conf.consumers {
		b := strings.Join(c.brokers, ",")
		if _, exists := clusters[b]; !exists {
			clusters[b] = &cluster{brokers: c.brokers}
		}
	}

	errors := setupClusters(clusters, kafkaUtils{})
	var errL sync.Mutex
	if len(errors) > 0 {
		log.Printf("%v error(s) while setting up consumers:", len(errors))
		for i, e := range errors {
			log.Printf("Error #%v: %v", i, e)
		}
		closeAll(clusters)
		return nil, nil, false
	}

	partitionConsumerChans := []<-chan *sarama.ConsumerMessage{}
	var pccL sync.Mutex

	var wg sync.WaitGroup

	for _, consumerConf := range conf.consumers {
		gowg(func(consumerConf consumerConfig) func() {
			return func() {
				topic, brokers, partition := consumerConf.topic, consumerConf.brokers, consumerConf.partition
				brokStr := strings.Join(brokers, ",")
				cluster := clusters[brokStr]
				client, consumer, pcL := cluster.client, cluster.consumer, cluster.pcL

				var partitions []int32
				if partition == -1 {
					var err error
					partitions, err = consumer.Partitions(topic)
					if err != nil {
						errL.Lock()
						errors = append(errors, fmt.Errorf("Error fetching partitions for topic. err=%v", err))
						errL.Unlock()
						return
					}
				} else {
					partitions = append(partitions, int32(partition))
				}

				for _, partition := range partitions {
					offset, err := resolveOffset(consumerConf.offset, brokers, topic, partition, client)
					if err != nil {
						errL.Lock()
						errors = append(errors, fmt.Errorf("Could not resolve offset for %v, %v, %v. err=%v", brokers, topic, partition, err))
						errL.Unlock()
						return
					}

					partitionConsumer, err := consumer.ConsumePartition(topic, int32(partition), offset)
					if err != nil {
						errL.Lock()
						errors = append(errors, fmt.Errorf("Failed to consume partition %v err=%v\n", partition, err))
						errL.Unlock()
						return
					}

					pcL.Lock()
					cluster.partitionConsumers = append(cluster.partitionConsumers, partitionConsumer)
					pcL.Unlock()
					pccL.Lock()
					partitionConsumerChans = append(partitionConsumerChans, partitionConsumer.Messages())
					pccL.Unlock()
				}
				log.Printf("Added %v partition consumer(s) for topic [%v]", len(partitions), topic)
			}
		}(consumerConf), &wg)
	}

	wg.Wait()

	if len(errors) > 0 {
		log.Printf("%v error(s) while setting up partition consumers:", len(errors))
		for i, e := range errors {
			log.Printf("Error #%v: %v", i, e)
		}
		closeAll(clusters)
		return nil, nil, false
	}

	log.Println("Successfully finished setting up partition consumers. Ready to consume, bro!")
	return partitionConsumerChans, clusters, true
}

type iClient interface {
	GetOffset(string, int32, int64) (int64, error)
	Close() error
}

type client struct{}

func (c client) GetOffset(topic string, partition int32, time int64) (int64, error) {
	return c.GetOffset(topic, partition, time)
}

func (c client) Close() error {
	return c.Close()
}

type iClientCreator interface {
	NewClient([]string) (iClient, error)
}

type clientCreator struct{}

func (s clientCreator) NewClient(brokers []string) (iClient, error) {
	return sarama.NewClient(brokers, nil)
}

func resolveOffset(configOffset string, brokers []string, topic string, partition int32, client sarama.Client) (int64, error) {
	if configOffset == "oldest" {
		return sarama.OffsetOldest, nil
	} else if configOffset == "newest" {
		return sarama.OffsetNewest, nil
	} else if numericOffset, err := strconv.ParseInt(configOffset, 10, 64); err == nil {
		if numericOffset >= -2 {
			return numericOffset, nil
		}

		oldest, err := client.GetOffset(topic, partition, sarama.OffsetOldest)
		if err != nil {
			return 0, err
		}

		newest, err := client.GetOffset(topic, partition, sarama.OffsetNewest)
		if err != nil {
			return 0, err
		}

		if newest+numericOffset < oldest {
			return oldest, nil
		}

		return newest + numericOffset, nil
	}

	return 0, fmt.Errorf("Invalid value for consumer offset")
}

func demuxMessages(pc []<-chan *sarama.ConsumerMessage, q chan struct{}) chan *sarama.ConsumerMessage {
	c := make(chan *sarama.ConsumerMessage)
	for _, p := range pc {
		go func(p <-chan *sarama.ConsumerMessage) {
			for {
				select {
				case msg := <-p:
					c <- msg
				case <-q:
					return
				}
			}
		}(p)
	}
	return c
}

type iSender interface {
	Send(*websocket.Conn, string) error
}

type sender struct{}

func (s sender) Send(ws *websocket.Conn, msg string) error {
	return websocket.Message.Send(ws, msg)
}

type iTimeNow interface {
	Unix() int64
}

type timeNow struct{}

func (t timeNow) Unix() int64 {
	return time.Now().Unix()
}

func sendMessagesToWsBlocking(ws *websocket.Conn, c chan *sarama.ConsumerMessage, q chan struct{}, sender iSender, timeNow iTimeNow) {
	for {
		select {
		case cMsg := <-c:
			msg :=
				`{"topic": "` + cMsg.Topic +
					`", "partition": "` + strconv.FormatInt(int64(cMsg.Partition), 10) +
					`", "offset": "` + strconv.FormatInt(cMsg.Offset, 10) +
					`", "key": "` + strings.Replace(string(cMsg.Key), `"`, `\"`, -1) +
					`", "value": "` + strings.Replace(string(cMsg.Value), `"`, `\"`, -1) +
					`", "consumedUnixTimestamp": "` + strconv.FormatInt(timeNow.Unix(), 10) +
					`"}` + "\n"

			err := sender.Send(ws, msg)
			if err != nil {
				log.Printf("Error while trying to send to WebSocket: err=%v\n", err)
				return
			}
		case <-q:
			log.Println("Received quit signal")
			return
		}
	}
}
