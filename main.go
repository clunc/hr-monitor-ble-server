package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"hr-monitor-ble-server/heartrate"
	"tinygo.org/x/bluetooth"
)

func main() {
	config, err := heartrate.LoadConfig("config.json")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	dataStream := make(chan heartrate.HeartRatePayload, 100) // Increased buffer size
	kafkaBroker := os.Getenv("KAFKA_BROKER")
	if kafkaBroker == "" {
		log.Fatal("KAFKA_BROKER environment variable is not set")
	}

	producer, err := createKafkaProducer(kafkaBroker, 5, 2*time.Second)
	if err != nil {
		log.Fatalf("Failed to create Kafka producer: %v", err)
	}
	defer producer.Close()

	topic := "sensor_data"

	go func() {
		for data := range dataStream {
			log.Printf("Received heart rate data: %d bpm at %s", data.HeartRate, data.Timestamp)
			sendToKafka(producer, topic, data, 3)
		}
		log.Println("Data stream channel closed")
	}()

	var peer *bluetooth.Device
	var characteristic bluetooth.DeviceCharacteristic

	connectToDevice := func() (*bluetooth.Device, bluetooth.DeviceCharacteristic, error) {
		if peer != nil {
			peer.Disconnect()
		}
		log.Println("Scanning and connecting to BLE device...")
		newPeer, err := heartrate.ScanAndConnect(config)
		if err != nil {
			return nil, bluetooth.DeviceCharacteristic{}, err
		}
		log.Println("Connected to BLE device")

		services, err := heartrate.DiscoverServices(newPeer)
		if err != nil {
			return nil, bluetooth.DeviceCharacteristic{}, err
		}

		characteristics, err := heartrate.DiscoverCharacteristics(services[0])
		if err != nil {
			return nil, bluetooth.DeviceCharacteristic{}, err
		}

		return newPeer, characteristics[0], nil
	}

	reconnect := func() error {
		var err error
		peer, characteristic, err = connectToDevice()
		if err != nil {
			return err
		}

		return heartrate.SubscribeHeartRateData(characteristic, dataStream)
	}

	if err := reconnect(); err != nil {
		log.Fatalf("Failed to set up initial connection: %v", err)
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if !heartrate.IsDeviceConnected(peer, characteristic) {
				log.Println("BLE device disconnected, attempting to reconnect...")
				if err := reconnect(); err != nil {
					log.Printf("Reconnection failed: %v", err)
				} else {
					log.Println("Reconnected to BLE device")
				}
			}
		}
	}()

	select {}
}

func createKafkaProducer(broker string, maxRetries int, retryInterval time.Duration) (*kafka.Producer, error) {
	var producer *kafka.Producer
	var err error

	for i := 0; i < maxRetries; i++ {
		producer, err = kafka.NewProducer(&kafka.ConfigMap{"bootstrap.servers": broker})
		if err == nil {
			log.Println("Connected to Kafka")
			return producer, nil
		}
		log.Printf("Failed to connect to Kafka, retrying in %v seconds...\n", retryInterval.Seconds())
		time.Sleep(retryInterval)
	}
	return nil, err
}

func sendToKafka(producer *kafka.Producer, topic string, data heartrate.HeartRatePayload, maxRetries int) {
	message, err := json.Marshal(data)
	if err != nil {
		log.Printf("Failed to marshal data: %v", err)
		return
	}

	for i := 0; i < maxRetries; i++ {
		err = producer.Produce(&kafka.Message{
			TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
			Value:          message,
		}, nil)

		if err == nil {
			log.Printf("Message sent to Kafka")
			return
		}

		log.Printf("Failed to produce message, retrying... (%d/%d)", i+1, maxRetries)
		time.Sleep(1 * time.Second)
	}

	log.Printf("Failed to produce message after %d retries: %v", maxRetries, err)
}
