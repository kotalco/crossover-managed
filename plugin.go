package crossover_managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/kotalco/crossover-managed/activity"
	"github.com/kotalco/crossover-managed/cache"
	"github.com/kotalco/crossover-managed/limiter"
	"github.com/kotalco/resp"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
)

const MaxRequestBodySize int64 = 2 * 1024 * 1024 // 2 MB

// implement buffer pool using the sync.Pool type,to reduce the allocation when you are encoding JSON
var cloneBufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// Config holds configuration to passed to the plugin
type Config struct {
	Pattern         string
	APIKey          string
	ActivityAddress string
	PlanAddress     string
	RedisAddress    string
	RedisAuth       string
	CacheExpiry     int
	BufferSize      int
	BatchSize       int
	FlushInterval   int
}

// CreateConfig populates the config data object
func CreateConfig() *Config {
	return &Config{}
}

type Crossover struct {
	next            http.Handler
	name            string
	compiledPattern *regexp.Regexp
	apiKey          string
	planAddress     string
	redisAddress    string
	redisAuth       string
	cacheExpiry     int
	activityService activity.IActivity
	cacheService    cache.ICache
	limiterService  limiter.ILimiter
}

// New created a new  plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.Pattern) == 0 {
		return nil, fmt.Errorf("pattern can't be empty")
	}
	if len(config.APIKey) == 0 {
		return nil, fmt.Errorf("APIKey can't be empty")
	}
	if len(config.ActivityAddress) == 0 {
		return nil, fmt.Errorf("activityAddress can't be empty")
	}
	if len(config.PlanAddress) == 0 {
		return nil, fmt.Errorf("planAddress can't be empty")
	}
	if len(config.RedisAddress) == 0 {
		return nil, fmt.Errorf("RedisAddress can't be empty")
	}
	if config.CacheExpiry == 0 {
		return nil, fmt.Errorf("cacheExpiry can't be empty")
	}
	if config.BufferSize == 0 {
		return nil, fmt.Errorf("bufferSize can't be empty")
	}
	if config.BatchSize == 0 {
		return nil, fmt.Errorf("batchSize can't be empty")
	}
	if config.FlushInterval == 0 {
		return nil, fmt.Errorf("flushInterval can't be empty")
	}

	compiledPattern := regexp.MustCompile(config.Pattern)
	//newActivityService
	newActivity := activity.NewActivity(
		config.ActivityAddress,
		config.APIKey,
		config.BufferSize,
		config.BatchSize,
		config.FlushInterval,
	)
	//cache service
	newCacheService := cache.NewCache(config.CacheExpiry)
	//limiter service
	newLimiterService := limiter.NewLimiter(config.APIKey, config.PlanAddress)

	handler := &Crossover{
		next:            next,
		name:            name,
		compiledPattern: compiledPattern,
		apiKey:          config.APIKey,
		planAddress:     config.PlanAddress,
		redisAddress:    config.RedisAddress,
		cacheExpiry:     config.CacheExpiry,
		activityService: newActivity,
		cacheService:    newCacheService,
		limiterService:  newLimiterService,
	}
	go handler.activityService.BatchProcessor()
	return handler, nil
}

func (crossover *Crossover) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	respClient, err := resp.NewRedisClient(crossover.redisAddress, crossover.redisAuth)
	if err != nil {
		log.Printf("Failed to create Redis Connection %s", err.Error())
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte("something went wrong"))
		return
	}
	defer respClient.Close()

	//extract user id from request
	userId := crossover.extractUserID(req.URL.Path)
	if userId == "" {
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write([]byte("invalid requestId"))
		return
	}

	//
	//limit user request according to his/her plan
	//
	newLimiter := crossover.limiterService
	allow, err := newLimiter.Limit(req.Context(), crossover.extractUserID(req.URL.Path), respClient)
	if err != nil {
		if err.Error() == http.StatusText(http.StatusTooManyRequests) {
			rw.WriteHeader(http.StatusTooManyRequests)
			rw.Write([]byte(err.Error()))
			return
		}
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte(err.Error()))
		return
	}
	if !allow {
		rw.WriteHeader(http.StatusTooManyRequests)
		rw.Write([]byte("too many requests"))
		return
	}

	//clone request
	clonedRequest, err := crossover.cloneRequest(req)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte(err.Error()))
		return
	}
	//
	//store user activity
	//count request body
	requestCount := crossover.requestCount(clonedRequest)
	requestKey := crossover.requestKey(req.URL.Path)
	crossover.activityService.LogActivity(requestKey, requestCount)

	//cache response
	crossover.cacheService.ServeHTTP(rw, req, crossover.next, respClient)
	return

	crossover.next.ServeHTTP(rw, req)
	return
}

// extractUserID extract user id from the request
func (crossover *Crossover) extractUserID(path string) (userId string) {
	// Find the first match of the pattern in the URL Path
	match := crossover.compiledPattern.FindStringSubmatch(path)
	if len(match) == 0 {
		return
	}
	parsedUUID, err := uuid.Parse(match[0][10:])
	if err != nil {
		return
	}
	return parsedUUID.String()
}

// requestKey return clone of the request
func (crossover *Crossover) requestKey(path string) string {
	match := crossover.compiledPattern.FindStringSubmatch(path)
	if len(match) == 0 {
		return ""
	}
	return match[0]
}

func (crossover *Crossover) cloneRequest(req *http.Request) (*http.Request, error) {
	buf := cloneBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer cloneBufferPool.Put(buf)

	// Limit the size of the request body that we will read
	//this will guard the plugin from malicious body request by users
	_, err := io.CopyN(buf, req.Body, MaxRequestBodySize)
	req.Body.Close()
	if err != nil && err != io.EOF {
		log.Printf("Error reading request body: %s", err)
		return nil, errors.New("error reading request body")
	}
	bodyReader := bytes.NewReader(buf.Bytes())
	req.Body = io.NopCloser(bodyReader)
	clonedRequest := req.Clone(req.Context())
	clonedRequest.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	return clonedRequest, nil
}

func (crossover *Crossover) requestCount(req *http.Request) (count int) {
	contentType := req.Header.Get("Content-Type")
	if contentType != "application/json" {
		// if it's not of type json default to 1 and return before reading the body
		return 1
	}

	decoder := json.NewDecoder(req.Body)
	var requests []interface{}
	err := decoder.Decode(&requests)

	io.Copy(io.Discard, req.Body)
	req.Body.Close()

	if err != nil {
		//if it fails to decode []objects assume it's a single object then return
		return 1
	}
	count = len(requests)
	return count
}
