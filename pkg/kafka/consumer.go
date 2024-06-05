package kafka

import (
    "context"
    "log"

    "github.com/segmentio/kafka-go"
)

func Consume(brokerAddress, topic string) error {
    r := kafka.NewReader(kafka.ReaderConfig{
        Brokers: []string{brokerAddress},
        Topic:   topic,
        GroupID: "consumer-group-id",
    })

    for {
        m, err := r.ReadMessage(context.Background())
        if err != nil {
            return err
        }
        log.Printf("message at offset %d: %s = %s\n", m.Offset, string(m.Key), string(m.Value))
    }
}
