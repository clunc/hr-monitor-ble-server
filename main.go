package main

import (
    "log"
    "os"
    "time"
    "encoding/json"

    "github.com/confluentinc/confluent-kafka-go/kafka"
    "hr-monitor-ble-server/heartrate"
)

func main() {
    config, err := heartrate.LoadConfig("config.json")
    if err != nil {
        log.Fatalf("Failed to load config: %v", err)
    }

    peer, err := heartrate.ScanAndConnect(config)
    if err != nil {
        log.Fatalf("Failed to scan and connect: %v", err)
    }
    defer peer.Disconnect()

    services, err := heartrate.DiscoverServices(peer)
    if err != nil {
        log.Fatalf("Failed to discover services: %v", err)
    }

    characteristics, err := heartrate.DiscoverCharacteristics(services[0])
    if err != nil {
        log.Fatalf("Failed to discover characteristics: %v", err)
    }

    dataStream := make(chan heartrate.HeartRatePayload)

    err = heartrate.SubscribeHeartRateData(characteristics[0], dataStream)
    if err != nil {
        log.Fatalf("Failed to subscribe to heart rate data: %v", err)
    }

    // Get Kafka broker address from environment variable
    kafkaBroker := os.Getenv("KAFKA_BROKER")
    if kafkaBroker == "" {
        log.Fatal("KAFKA_BROKER environment variable is not set")
    }

    // Set up Kafka producer with retry logic
    var producer *kafka.Producer
    maxRetries := 5
    retryInterval := 2 * time.Second

    for i := 0; i < maxRetries; i++ {
        producer, err = kafka.NewProducer(&kafka.ConfigMap{"bootstrap.servers": kafkaBroker})
        if err == nil {
            log.Println("Connected to Kafka")
            break
        }
        log.Printf("Failed to connect to Kafka, retrying in %v seconds...\n", retryInterval.Seconds())
        time.Sleep(retryInterval)
    }

    if err != nil {
        log.Fatalf("Failed to create Kafka producer after %d retries: %v", maxRetries, err)
    }
    defer producer.Close()

    topic := "sensor_data"

    go func() {
        for data := range dataStream {
            log.Printf("Received heart rate data: %d bpm at %s", data.HeartRate, data.Timestamp)

            // Prepare the message to be sent to Kafka
            message, err := json.Marshal(data)
            if err != nil {
                log.Printf("Failed to marshal data: %v", err)
                continue
            }

            // Send the message to Kafka
            err = producer.Produce(&kafka.Message{
                TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
                Value:          message,
            }, nil)

            if err != nil {
                log.Printf("Failed to produce message: %v", err)
            }
        }
    }()

    // Keep the program running.
    select {}
}
