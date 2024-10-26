package main

import (
    "encoding/json"
    "log"
    "os"

    "github.com/joho/godotenv"
    "github.com/IBM/sarama"
    "github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
)

func main() {
    // Load environment variables from .env file
    err := godotenv.Load(".env")
    if err != nil {
        log.Fatalf("Error loading .env file: %v", err)
    }

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

    producer, err := createKafkaProducer(kafkaBroker)
    if err != nil {
        log.Fatalf("Failed to create Kafka producer: %v", err)
    }
    defer producer.Close()

    topic := os.Getenv("TOPIC")
    if topic == "" {
        log.Fatal("TOPIC environment variable is not set")
    }

    // Kafka producer goroutine
    go func() {
        for data := range dataStream {
            log.Printf("Received heart rate data: %d bpm at %s", data.HeartRate, data.Timestamp)
            sendToKafka(producer, topic, data)
        }
        log.Println("Data stream channel closed")
    }()

    // Keep the main function running
    select {}
}

func createKafkaProducer(broker string) (sarama.SyncProducer, error) {
    config := sarama.NewConfig()
    config.Producer.Retry.Max = 5
    config.Producer.RequiredAcks = sarama.WaitForAll
    config.Producer.Return.Successes = true

    producer, err := sarama.NewSyncProducer([]string{broker}, config)
    if err != nil {
        return nil, err
    }

    log.Println("Connected to Kafka")
    return producer, nil
}

func sendToKafka(producer sarama.SyncProducer, topic string, data heartrate.HeartRatePayload) {
    message, err := json.Marshal(data)
    if err != nil {
        log.Printf("Failed to marshal data: %v", err)
        return
    }

    msg := &sarama.ProducerMessage{
        Topic: topic,
        Value: sarama.ByteEncoder(message),
    }

    partition, offset, err := producer.SendMessage(msg)
    if err != nil {
        log.Printf("Failed to produce message: %v", err)
        return
    }

    log.Printf("Message sent to Kafka partition %d, offset %d", partition, offset)
}

