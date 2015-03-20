/* Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License. */

package go_kafka_client

import (
	"fmt"
	avro "github.com/stealthly/go-avro"
	"hash/fnv"
	"time"
)

var TimingField = &avro.SchemaField{
	Name:    "timings",
	Doc:     "Timings",
	Default: "null",
	Type: &avro.UnionSchema{
		Types: []avro.Schema{
			&avro.NullSchema{},
			&avro.ArraySchema{
				Items: &avro.LongSchema{},
			},
		},
	},
}

// MirrorMakerConfig defines configuration options for MirrorMaker
type MirrorMakerConfig struct {
	// Whitelist of topics to mirror. Exactly one whitelist or blacklist is allowed.
	Whitelist string

	// Blacklist of topics to mirror. Exactly one whitelist or blacklist is allowed.
	Blacklist string

	// Consumer configurations to consume from a source cluster.
	ConsumerConfigs []string

	// Embedded producer config.
	ProducerConfig string

	// Number of producer instances.
	NumProducers int

	// Number of consumption streams.
	NumStreams int

	// Flag to preserve partition number. E.g. if message was read from partition 5 it'll be written to partition 5. Note that this can affect performance.
	PreservePartitions bool

	// Flag to preserve message order. E.g. message sequence 1, 2, 3, 4, 5 will remain 1, 2, 3, 4, 5 in destination topic. Note that this can affect performance.
	PreserveOrder bool

	// Destination topic prefix. E.g. if message was read from topic "test" and prefix is "dc1_" it'll be written to topic "dc1_test".
	TopicPrefix string

	// Number of messages that are buffered between the consumer and producer.
	ChannelSize int

	// Message keys encoder for producer
	KeyEncoder Encoder

	// Message values encoder for producer
	ValueEncoder Encoder

	// Message keys decoder for consumer
	KeyDecoder Decoder

	// Message values decoder for consumer
	ValueDecoder Decoder

	// Function that generates producer instances
	ProducerConstructor ProducerConstructor

	// Path to producer configuration, that is responsible for logging timings
	// Defines whether add timings to message or not.
	// Note: used only for avro encoded messages
	TimingsProducerConfig string
}

// Creates an empty MirrorMakerConfig.
func NewMirrorMakerConfig() *MirrorMakerConfig {
	return &MirrorMakerConfig{
		KeyEncoder:            &ByteEncoder{},
		ValueEncoder:          &ByteEncoder{},
		KeyDecoder:            &ByteDecoder{},
		ValueDecoder:          &ByteDecoder{},
		ProducerConstructor:   NewSaramaProducer,
		TimingsProducerConfig: "",
	}
}

// MirrorMaker is a tool to mirror source Kafka cluster into a target (mirror) Kafka cluster.
// It uses a Kafka consumer to consume messages from the source cluster, and re-publishes those messages to the target cluster.
type MirrorMaker struct {
	config          *MirrorMakerConfig
	consumers       []*Consumer
	producers       []Producer
	messageChannels []chan *Message
	timingsProducer Producer
	newSchema       *avro.RecordSchema
}

// Creates a new MirrorMaker using given MirrorMakerConfig.
func NewMirrorMaker(config *MirrorMakerConfig) *MirrorMaker {
	return &MirrorMaker{
		config: config,
	}
}

// Starts the MirrorMaker. This method is blocking and should probably be run in a separate goroutine.
func (this *MirrorMaker) Start() {
	this.initializeMessageChannels()
	this.startConsumers()
	this.startProducers()
}

// Gracefully stops the MirrorMaker.
func (this *MirrorMaker) Stop() {
	consumerCloseChannels := make([]<-chan bool, 0)
	for _, consumer := range this.consumers {
		consumerCloseChannels = append(consumerCloseChannels, consumer.Close())
	}

	for _, ch := range consumerCloseChannels {
		<-ch
	}

	for _, ch := range this.messageChannels {
		close(ch)
	}

	//TODO maybe drain message channel first?
	for _, producer := range this.producers {
		producer.Close()
	}
}

func (this *MirrorMaker) startConsumers() {
	for _, consumerConfigFile := range this.config.ConsumerConfigs {
		config, err := ConsumerConfigFromFile(consumerConfigFile)
		if err != nil {
			panic(err)
		}
		config.KeyDecoder = this.config.KeyDecoder
		config.ValueDecoder = this.config.ValueDecoder

		zkConfig, err := ZookeeperConfigFromFile(consumerConfigFile)
		if err != nil {
			panic(err)
		}
		config.NumWorkers = 1
		config.AutoOffsetReset = SmallestOffset
		config.Coordinator = NewZookeeperCoordinator(zkConfig)
		config.WorkerFailureCallback = func(_ *WorkerManager) FailedDecision {
			return CommitOffsetAndContinue
		}
		config.WorkerFailedAttemptCallback = func(_ *Task, _ WorkerResult) FailedDecision {
			return CommitOffsetAndContinue
		}
		if this.config.PreserveOrder {
			numProducers := this.config.NumProducers
			config.Strategy = func(_ *Worker, msg *Message, id TaskId) WorkerResult {
				if this.config.TimingsProducerConfig != "" {
					if record, ok := msg.DecodedValue.(*avro.GenericRecord); ok {
						msg.DecodedValue = this.addTiming(record)
						return NewSuccessfulResult(id)
					} else {
						return NewProcessingFailedResult(id)
					}
				}

				this.messageChannels[topicPartitionHash(msg)%numProducers] <- msg

				return NewSuccessfulResult(id)
			}
		} else {
			config.Strategy = func(_ *Worker, msg *Message, id TaskId) WorkerResult {
				this.messageChannels[0] <- msg

				return NewSuccessfulResult(id)
			}
		}

		consumer := NewConsumer(config)
		this.consumers = append(this.consumers, consumer)
		if this.config.Whitelist != "" {
			go consumer.StartWildcard(NewWhiteList(this.config.Whitelist), this.config.NumStreams)
		} else if this.config.Blacklist != "" {
			go consumer.StartWildcard(NewBlackList(this.config.Blacklist), this.config.NumStreams)
		} else {
			panic("Consume pattern not specified")
		}
	}
}

func (this *MirrorMaker) initializeMessageChannels() {
	if this.config.PreserveOrder {
		for i := 0; i < this.config.NumProducers; i++ {
			this.messageChannels = append(this.messageChannels, make(chan *Message, this.config.ChannelSize))
		}
	} else {
		this.messageChannels = append(this.messageChannels, make(chan *Message, this.config.ChannelSize))
	}
}

func (this *MirrorMaker) startProducers() {
	if this.config.TimingsProducerConfig != "" {
		conf, err := ProducerConfigFromFile(this.config.TimingsProducerConfig)
		if err != nil {
			panic(err)
		}
		if this.config.PreservePartitions {
			conf.Partitioner = NewFixedPartitioner
		} else {
			conf.Partitioner = NewRandomPartitioner
		}
		conf.KeyEncoder = this.config.KeyEncoder
		conf.ValueEncoder = this.config.ValueEncoder
		this.timingsProducer = this.config.ProducerConstructor(conf)
		go this.failedRoutine(this.timingsProducer)
	}

	for i := 0; i < this.config.NumProducers; i++ {
		conf, err := ProducerConfigFromFile(this.config.ProducerConfig)
		if err != nil {
			panic(err)
		}
		if this.config.PreservePartitions {
			conf.Partitioner = NewFixedPartitioner
		} else {
			conf.Partitioner = NewRandomPartitioner
		}
		conf.KeyEncoder = this.config.KeyEncoder
		conf.ValueEncoder = this.config.ValueEncoder
		producer := this.config.ProducerConstructor(conf)
		this.producers = append(this.producers, producer)
		if this.config.TimingsProducerConfig != "" {
			go this.timingsRoutine(producer)
		}
		go this.failedRoutine(producer)
		if this.config.PreserveOrder {
			go this.produceRoutine(producer, i)
		} else {
			go this.produceRoutine(producer, 0)
		}
	}
}

func (this *MirrorMaker) produceRoutine(producer Producer, channelIndex int) {
	partitionEncoder := &Int32Encoder{}
	for msg := range this.messageChannels[channelIndex] {
		if this.config.PreservePartitions {
			producer.Input() <- &ProducerMessage{Topic: this.config.TopicPrefix + msg.Topic, Key: uint32(msg.Partition), Value: msg.DecodedValue, KeyEncoder: partitionEncoder}
		} else {
			producer.Input() <- &ProducerMessage{Topic: this.config.TopicPrefix + msg.Topic, Key: msg.Key, Value: msg.DecodedValue}
		}
	}
}

func (this *MirrorMaker) timingsRoutine(producer Producer) {
	partitionEncoder := &Int32Encoder{}
	partitionDecoder := &Int32Decoder{}
	for msg := range producer.Successes() {
		decodedKey, err := partitionDecoder.Decode(msg.Key.([]byte))
		if err != nil {
			Errorf(this, "Failed to decode %v", msg.Key)
		}
		decodedValue, err := this.config.ValueDecoder.Decode(msg.Value.([]byte))
		if err != nil {
			Errorf(this, "Failed to decode %v", msg.Value)
		}

		if record, ok := decodedValue.(*avro.GenericRecord); ok {
			record = this.addTiming(record)
			this.timingsProducer.Input() <- &ProducerMessage{Topic: "timings_" + msg.Topic, Key: decodedKey.(uint32),
				Value: record, KeyEncoder: partitionEncoder}
		} else {
			Errorf(this, "Invalid avro schema type %s", decodedValue)
		}
	}
}

func (this *MirrorMaker) failedRoutine(producer Producer) {
	for msg := range producer.Errors() {
		Error("mirrormaker", msg.err)
	}
}

func (this *MirrorMaker) addTiming(record *avro.GenericRecord) *avro.GenericRecord {
	now := time.Now().Unix()
	if this.newSchema == nil {
		schema := *record.Schema().(*avro.RecordSchema)
		this.newSchema = &schema
		this.newSchema.Fields = append(this.newSchema.Fields, TimingField)
	}
	var timings []interface{}
	if record.Get("timings") == nil {
		timings = make([]interface{}, 0)
		newRecord := avro.NewGenericRecord(this.newSchema)
		for _, field := range this.newSchema.Fields {
			newRecord.Set(field.Name, record.Get(field.Name))
		}
		record = newRecord
	} else {
		timings = record.Get("timings").([]interface{})
	}
	timings = append(timings, now)
	record.Set("timings", timings)

	return record
}

func topicPartitionHash(msg *Message) int {
	h := fnv.New32a()
	h.Write([]byte(fmt.Sprintf("%s%d", msg.Topic, msg.Partition)))
	return int(h.Sum32())
}
