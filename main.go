package main

import (
	"encoding/json"
	"os"

	"github.com/IBM/sarama"
	"github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
	"github.com/sirupsen/logrus"
)

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	config := heartrate.Config{
		TargetDeviceName: "Polar H10",
		ScanTimeout:      30,
	}

	hrm := heartrate.NewHeartRateMonitor(config)
	dataStream := hrm.Subscribe()
	hrm.Start()
	defer hrm.Stop()

	kafkaBroker := os.Getenv("KAFKA_BROKER")
	if kafkaBroker == "" {
		logrus.Fatal("KAFKA_BROKER environment variable is not set")
	}

	topic := os.Getenv("TOPIC")
	if topic == "" {
		logrus.Fatal("TOPIC environment variable is not set")
	}

	producer, err := createKafkaProducer(kafkaBroker)
	if err != nil {
		logrus.Fatalf("Failed to create Kafka producer: %v", err)
	}
	defer producer.Close()
	logrus.Infof("Connected to Kafka broker %s, publishing to topic %s", kafkaBroker, topic)

	for data := range dataStream {
		if len(data.RRIntervals) > 0 {
			logrus.Infof("Heart rate: %d bpm | RR: %v ms", data.HeartRate, data.RRIntervals)
		} else {
			logrus.Infof("Heart rate: %d bpm", data.HeartRate)
		}
		sendToKafka(producer, topic, data)
	}
}

func createKafkaProducer(broker string) (sarama.SyncProducer, error) {
	config := sarama.NewConfig()
	config.Producer.Retry.Max = 5
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Return.Successes = true

	return sarama.NewSyncProducer([]string{broker}, config)
}

func sendToKafka(producer sarama.SyncProducer, topic string, data heartrate.HeartRatePayload) {
	message, err := json.Marshal(data)
	if err != nil {
		logrus.Errorf("Failed to marshal payload: %v", err)
		return
	}

	_, _, err = producer.SendMessage(&sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.ByteEncoder(message),
	})
	if err != nil {
		logrus.Errorf("Failed to publish to Kafka: %v", err)
	}
}
