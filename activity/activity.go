package activity

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const DefaultTimeout = 10

// loggingRequestDto used to send request to the third party to save no of requests
type activityRequestDto struct {
	RequestId string `json:"request_id"`
	Count     int    `json:"count"`
}

// implement buffer pool using the sync.Pool type,to reduce the allocation when you are encoding JSON
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

type IActivity interface {
	LogActivity(requestId string, count int)
	BatchProcessor()
	FlushLogs(batch []activityRequestDto)
}

type activity struct {
	client        *http.Client
	logsChannel   chan activityRequestDto
	remoteAddress string
	apiKey        string
	batchSize     int
	flushInterval int
}

func NewActivity(remoteAddress string, apiKey string, bufferSize int, batchSize int, flushInterval int) IActivity {
	return &activity{
		client: &http.Client{
			Timeout: DefaultTimeout * time.Second,
		},
		logsChannel:   make(chan activityRequestDto, bufferSize),
		remoteAddress: remoteAddress,
		apiKey:        apiKey,
		batchSize:     batchSize,
		flushInterval: flushInterval,
	}
}

func (a *activity) LogActivity(requestId string, count int) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// Create log entry
	logEntry := activityRequestDto{
		RequestId: requestId,
		Count:     count,
	}

	//send logEntry to logsChannel with select and don't block
	select {
	case a.logsChannel <- logEntry:
	default:
		log.Printf("Dropped some log entries due to full buffer channel")
	}

}

// BatchProcessor runs in a separate goroutine and batches logs.
func (a *activity) BatchProcessor() {
	var batch []activityRequestDto
	flushTimer := time.NewTimer(time.Duration(a.flushInterval) * time.Second)
	for {
		select {
		case logEntry := <-a.logsChannel:
			batch = append(batch, logEntry)
			if len(batch) >= a.batchSize {
				a.FlushLogs(batch)
				batch = nil // clear the batch
			}
		case <-flushTimer.C:
			if len(batch) > 0 {
				a.FlushLogs(batch)
				batch = nil // clear the batch
			}
			flushTimer.Reset(time.Duration(a.flushInterval) * time.Second)
		}
	}
}

// FlushLogs sends a batch of logs to the database.
func (a *activity) FlushLogs(batch []activityRequestDto) {
	// Aggregate the data and send it to the database in batches
	// Get a buffer from the pool and reset it back
	buffer := bufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	defer bufferPool.Put(buffer)
	//
	encoder := json.NewEncoder(buffer)
	err := encoder.Encode(batch)
	if err != nil {
		log.Printf("FLUSH_LOGS: %s", err.Error())
		return
	}
	//log.Println(a.remoteAddress, buffer)
	httpReq, err := http.NewRequest(http.MethodPost, a.remoteAddress, buffer)
	if err != nil {
		log.Printf("FLUSH_LOGS: %s", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", a.apiKey)

	httpRes, err := a.client.Do(httpReq)
	defer httpRes.Body.Close()
	if err != nil {
		log.Printf("FLUSH_LOGS: %s", err.Error())
		return
	}

	if httpRes.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(httpRes.Body)
		log.Printf("unexpected status code: %d, body: %s", httpRes.StatusCode, string(bodyBytes))
		return
	}

}
