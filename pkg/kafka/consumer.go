package main

import (
    "encoding/json"
    "log"
    "os"
    "time"

    "github.com/confluentinc/confluent-kafka-go/kafka"
)

type HeartRateData struct {
    HeartRate int       `json:"heart_rate"`
    Timestamp time.Time `json:"timestamp"`
}

func main() {
    brokerAddress := os.Getenv("BROKER_ADDRESS")
    topic := os.Getenv("TOPIC")

    if brokerAddress == "" {
        brokerAddress = "thinkcentre-janik:9092" // Default value for testing
    }
    if topic == "" {
        topic = "sensor_data" // Default value for testing
    }

    log.Println("Broker Address: ", brokerAddress)
    log.Println("Topic: ", topic)

    if err := ConsumeWithLatencyTracking(brokerAddress, topic); err != nil {
        log.Fatalf("Error consuming messages: %v", err)
    }
}

// ConsumeWithLatencyTracking consumes messages from Kafka and tracks message latency.
func ConsumeWithLatencyTracking(brokerAddress, topic string) error {
    config := kafka.ConfigMap{
        "bootstrap.servers":  brokerAddress,
        "group.id":           "my-consumer-group",
        "auto.offset.reset":  "earliest",
        "enable.auto.commit": false,
    }

    log.Printf("Kafka Config: %v", config)

    consumer, err := kafka.NewConsumer(&config)
    if err != nil {
        return err
    }
    defer consumer.Close()

    if err := consumer.SubscribeTopics([]string{topic}, nil); err != nil {
        return err
    }

    log.Printf("Consumer started. Waiting for messages...")

    for {
        select {
        case <-time.After(5 * time.Second): // Adjust the polling interval as needed
            continue
        default:
            msg, err := consumer.ReadMessage(-1)
            if err != nil {
                log.Printf("Error reading message: %v", err)
                continue
            }
            processMessageWithLatency(msg)
        }
    }
}

func processMessageWithLatency(msg *kafka.Message) {
    receivedTime := time.Now()

    var data HeartRateData
    if err := json.Unmarshal(msg.Value, &data); err != nil {
        log.Printf("Error unmarshaling message value: %v", err)
        return
    }

    latency := receivedTime.Sub(data.Timestamp)
    log.Printf("Message at offset %d: HeartRate = %d, Timestamp = %s, Latency = %s", msg.TopicPartition.Offset, data.HeartRate, data.Timestamp, latency)
}
