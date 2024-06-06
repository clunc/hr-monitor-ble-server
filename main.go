package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"hr-monitor-ble-server/pkg/heartrate"
)

func main() {
	// Define the heart rate monitor configuration
	config := heartrate.Config{
		TargetDeviceName: "Polar H10",
		ScanTimeout:      30,
	}

	// Initialize the heart rate monitor
	hrm := heartrate.NewHeartRateMonitor(config)
	dataStream := hrm.Subscribe()
	hrm.Start()
	defer hrm.Stop()

	// Kafka setup
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

	// Kafka producer goroutine
	go func() {
		for data := range dataStream {
			log.Printf("Received heart rate data: %d bpm at %s", data.HeartRate, data.Timestamp)
			sendToKafka(producer, topic, data, 3)
		}
		log.Println("Data stream channel closed")
	}()

	// Keep the main function running
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
