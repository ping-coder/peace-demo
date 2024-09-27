package main

import (
    "bufio"
    "context"
    "crypto/rand"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "os"
    "strings"
    "time"
    "cloud.google.com/go/bigquery"
    "github.com/confluentinc/confluent-kafka-go/kafka"
    "github.com/google/uuid"
    "log"
    "runtime/debug"
    "flag"
)

type BatchResult struct {
    BatchNumber int `json:"batch_number"`
    SendTime    int `json:"send_time"` // Send time in milliseconds
}

type BenchmarkResult struct {
    UUID        string    `json:"uuid"`
    NumMessages int       `json:"num_messages"`
    QPS         int       `json:"qps"`
    Duration    int       `json:"duration"` // Total duration in milliseconds
    SendTimes   []int     `json:"send_times"` // Send times for each batch in milliseconds
    Timestamp   time.Time `json:"timestamp"` // 新增的 timestamp 字段
}

type Message struct {
    UUID    string `json:"uuid"`
    Index   int    `json:"index"`
    Payload string `json:"payload"`
}

func loadConfig(filePath string) (map[string]string, error) {
    config := make(map[string]string)
    file, err := os.Open(filePath)
    if err != nil {
        return nil, err
    }
    defer file.Close()

    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := scanner.Text()
        if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
            continue
        }
        parts := strings.SplitN(line, "=", 2)
        if len(parts) == 2 {
            config[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
        }
    }
    return config, scanner.Err()
}

func generateFixedSizeMessage(id uuid.UUID, index int, size int) []byte {
    message := Message{
        UUID:  id.String(),
        Index: index,
    }

    // 计算需要的payload大小
    jsonPrefix, _ := json.Marshal(message)
    payloadSize := size - len(jsonPrefix) - 2 // 减去2是为了考虑到payload字段的引号

    if payloadSize < 0 {
        panic("Message size too small for JSON structure")
    }

    payload := make([]byte, payloadSize/2) // 因为hex编码后长度会翻倍
    _, err := rand.Read(payload)
    if err != nil {
        panic(err)
    }
    message.Payload = hex.EncodeToString(payload)

    jsonMessage, err := json.Marshal(message)
    if err != nil {
        panic(err)
    }

    // 如果生成的消息大小不足size，用空格填充
    if len(jsonMessage) < size {
        jsonMessage = append(jsonMessage, make([]byte, size-len(jsonMessage))...)
    }

    return jsonMessage[:size]
}

func produceMessages(producer *kafka.Producer, topic string, numMessages, qps, messageSize int) ([]int, int, error) {
    id := uuid.New()
    batchSize := qps
    numBatches := numMessages / batchSize
    if numMessages % batchSize != 0 {
        numBatches++
    }

    sendTimes := make([]int, numBatches)
    totalStart := time.Now()

    for batch := 0; batch < numBatches; batch++ {
        batchStart := time.Now()
        for i := 0; i < batchSize && batch*batchSize+i < numMessages; i++ {
            message := generateFixedSizeMessage(id, batch*batchSize+i, messageSize)
            err := producer.Produce(&kafka.Message{
                TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
                Value:          message,
            }, nil)

            if err != nil {
                log.Printf("Error producing message: %v\nStack trace:\n%s", err, debug.Stack())
                if err.(kafka.Error).Code() == kafka.ErrQueueFull {
                    log.Printf("Producer queue is full, waiting before retrying...\nStack trace:\n%s", debug.Stack())
                    time.Sleep(1 * time.Second)
                    i-- // 重试这条消息
                    continue
                }
                return nil, 0, fmt.Errorf("failed to produce message: %v\nStack trace:\n%s", err, debug.Stack())
            }
        }

        sendTimes[batch] += int(time.Since(batchStart).Milliseconds())
        elapsed := time.Since(batchStart)
        if elapsed < time.Second {
            time.Sleep(time.Second - elapsed)
        }
        
        log.Printf("Batch %d/%d completed.\n", batch+1, numBatches)
    }

    // 增加刷新超时时间
    remainingMessages := producer.Flush(60 * 1000)
    if remainingMessages > 0 {
        log.Printf("Warning: %d messages were not delivered\nStack trace:\n%s", remainingMessages, debug.Stack())
    }else{
        log.Printf("Info: %d messages were delivered\n", remainingMessages)
    }

    totalDuration := int(time.Since(totalStart).Milliseconds())
    return sendTimes, totalDuration, nil
}

func runBenchmark(broker, topic string, numMessages, qps, messageSize int, config map[string]string) (BenchmarkResult, error) {
    producer, err := kafka.NewProducer(&kafka.ConfigMap{
        "bootstrap.servers": broker,
        "security.protocol": config["security.protocol"],
        "sasl.mechanism":    config["sasl.mechanism"],
        "sasl.username":     config["username"],
        "sasl.password":     config["password"],
        "message.timeout.ms": "60000",  // 增加到60秒
        "acks":               "0", 
    })
    if err != nil {
        log.Printf("Failed to create producer: %v\nStack trace:\n%s", err, debug.Stack())
        return BenchmarkResult{}, fmt.Errorf("failed to create producer: %v", err)
    }
    defer producer.Close()

    go func() {
        for e := range producer.Events() {
            switch ev := e.(type) {
            case *kafka.Message:
                if ev.TopicPartition.Error != nil {
                    log.Printf("消息传递失败: %v\n主题: %s\n分区: %d\n偏移量: %d\n堆栈跟踪:\n%s", 
                        ev.TopicPartition.Error, *ev.TopicPartition.Topic, 
                        ev.TopicPartition.Partition, ev.TopicPartition.Offset, debug.Stack())
                }
            case kafka.Error:
                log.Printf("Kafka错误: %v\n堆栈跟踪:\n%s", ev, debug.Stack())
            default:
                log.Printf("忽略事件: %s\n", ev)
            }
        }
    }()

    metadata, err := producer.GetMetadata(&topic, false, 5000)
    if err != nil {
        log.Printf("Failed to get metadata: %v\nStack trace:\n%s", err, debug.Stack())
        return BenchmarkResult{}, fmt.Errorf("failed to get metadata: %v", err)
    }
    if _, exists := metadata.Topics[topic]; !exists {
        log.Printf("Topic %s does not exist\nStack trace:\n%s", topic, debug.Stack())
        return BenchmarkResult{}, fmt.Errorf("topic %s does not exist", topic)
    }

    sendTimes, totalDuration, err := produceMessages(producer, topic, numMessages, qps, messageSize)
    if err != nil {
        log.Printf("Error in produceMessages: %v\nStack trace:\n%s", err, debug.Stack())
        return BenchmarkResult{}, err
    }

    return BenchmarkResult{
        UUID:        uuid.New().String(),
        NumMessages: numMessages,
        QPS:         qps,
        Duration:    totalDuration,
        SendTimes:   sendTimes,
        Timestamp:   time.Now(),
    }, nil
}

func writeToBigQuery(projectID, datasetID, tableID string, data BenchmarkResult) {
    ctx := context.Background()
    client, err := bigquery.NewClient(ctx, projectID)
    if err != nil {
        panic(err)
    }
    defer client.Close()

    inserter := client.Dataset(datasetID).Table(tableID).Inserter()
    if err := inserter.Put(ctx, data); err != nil {
        fmt.Printf("Failed to insert data into BigQuery: %v\n", err)
    } else {
        fmt.Println("New rows have been added.")
    }
}

func main() {
    broker := flag.String("broker", "bootstrap.demo-etl-kafka-instance.us-central1.managedkafka.peace-demo.cloud.goog:9092", "Kafka broker address")
    topic := flag.String("topic", "benchmark_1", "Kafka topic")
    duration := flag.Int("duration", 300, "Duration of the benchmark in seconds")
    qps := flag.Int("qps", 100000, "Messages per second")
    messageSize := flag.Int("size", 128, "Message size in bytes")
    configPath := flag.String("config", "/home/client.properties", "Path to client properties file")

    flag.Parse()

    numMessages := *duration * *qps

    config, err := loadConfig(*configPath)
    if err != nil {
        log.Fatalf("Failed to load config: %v\nStack trace:\n%s", err, debug.Stack())
        return
    }

    log.Printf("Starting benchmark: Duration: %d seconds, QPS: %d, Total messages: %d, Message size: %d bytes\n", 
               *duration, *qps, numMessages, *messageSize)

    result, err := runBenchmark(*broker, *topic, numMessages, *qps, *messageSize, config)
    if err != nil {
        log.Fatalf("Benchmark failed: %v\nStack trace:\n%s", err, debug.Stack())
        return
    }

    fmt.Printf("Benchmark result: %+v\n", result)

    projectID := "peace-demo"
    datasetID := "us_benchmark"
    tableID := "kafka_benchmark_producer"

    writeToBigQuery(projectID, datasetID, tableID, result)
}