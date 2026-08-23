package main

import (
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
	"github.com/clunc/hr-monitor-ble-server/pkg/heartratepb"
	"github.com/clunc/hr-monitor-ble-server/pkg/httpapi"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	// Labels this gateway's beats so two straps can share one topic. Defaults to
	// a slug of the target name, which is right for the common single-strap case.
	source := envOr("HR_SOURCE", slug(envOr("TARGET_NAME", "Polar H10")))
	config := heartrate.Config{
		TargetDeviceName: envOr("TARGET_NAME", "Polar H10"),
		TargetDeviceMAC:  os.Getenv("TARGET_MAC"),
		ScanTimeout:      30,
	}

	hrm := heartrate.NewHeartRateMonitor(config)
	dataStream := hrm.Subscribe()
	hrm.Start()
	defer hrm.Stop()

	// Control API. The gateway now boots idle: the radio stays untouched until
	// something POSTs /connect (the dashboard toggle, or AUTO_CONNECT here).
	api := httpapi.New(hrm)
	addr := envOr("HTTP_ADDR", ":8080")
	go func() {
		logrus.Infof("control API listening on %s", addr)
		if err := http.ListenAndServe(addr, api.Handler()); err != nil {
			logrus.Fatalf("control API: %v", err)
		}
	}()
	if os.Getenv("AUTO_CONNECT") == "true" {
		logrus.Info("AUTO_CONNECT=true — acquiring link at boot")
		_ = hrm.Connect("", "")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		hrm.Stop()
		os.Exit(0)
	}()

	// Kafka is optional so the gateway can be exercised (and its control surface
	// tested) without a broker; measurements are logged either way.
	var producer sarama.SyncProducer
	topic := os.Getenv("TOPIC")
	if broker := os.Getenv("KAFKA_BROKER"); broker != "" && topic != "" {
		p, err := createKafkaProducer(broker)
		if err != nil {
			logrus.Fatalf("Failed to create Kafka producer: %v", err)
		}
		defer p.Close()
		producer = p
		logrus.Infof("Connected to Kafka broker %s, publishing to topic %s as source %q", broker, topic, source)
	} else {
		logrus.Warn("KAFKA_BROKER/TOPIC unset — running without a producer (log only)")
	}

	for data := range dataStream {
		data.Source = source
		if len(data.GetRrIntervals()) > 0 {
			logrus.Infof("Heart rate: %d bpm | RR: %v ms", data.GetHeartRate(), data.GetRrIntervals())
		} else {
			logrus.Infof("Heart rate: %d bpm", data.GetHeartRate())
		}
		if producer != nil {
			sendToKafka(producer, topic, data)
		}
	}
}

// slug turns "Polar H10" into "polar-h10" for use as a source label.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func createKafkaProducer(broker string) (sarama.SyncProducer, error) {
	config := sarama.NewConfig()
	config.Producer.Retry.Max = 5
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Return.Successes = true
	config.Metadata.Retry.Backoff = 2 * time.Second

	return sarama.NewSyncProducer([]string{broker}, config)
}

func sendToKafka(producer sarama.SyncProducer, topic string, data *heartratepb.HeartRateMeasurement) {
	message, err := proto.Marshal(data)
	if err != nil {
		logrus.Errorf("Failed to marshal measurement: %v", err)
		return
	}
	if _, _, err := producer.SendMessage(&sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.ByteEncoder(message),
	}); err != nil {
		logrus.Errorf("Failed to publish to Kafka: %v", err)
	}
}
