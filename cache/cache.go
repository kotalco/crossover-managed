package cache

import (
	"bytes"
	"encoding/gob"
	"github.com/kotalco/resp"
	"log"
	"net/http"
)

type CachedResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

type ICache interface {
	ServeHTTP(rw http.ResponseWriter, req *http.Request, next http.Handler, respClient resp.IClient)
}

type cache struct {
	cacheExpiry int
}

func NewCache(cacheExpiry int) ICache {
	gob.Register(CachedResponse{})
	return &cache{
		cacheExpiry: cacheExpiry,
	}

}
func (c *cache) ServeHTTP(rw http.ResponseWriter, req *http.Request, next http.Handler, respClient resp.IClient) {
	// cache key based on the request
	cacheKey := req.URL.Path

	// retrieve the cached response
	cachedData, err := respClient.Get(req.Context(), cacheKey)
	if err == nil && cachedData != "" {
		// Cache hit - parse the cached response and write it to the original ResponseWriter
		var cachedResponse CachedResponse
		buffer := bytes.NewBufferString(cachedData)
		dec := gob.NewDecoder(buffer)
		if err := dec.Decode(&cachedResponse); err == nil {
			for key, values := range cachedResponse.Headers {
				for _, value := range values {
					rw.Header().Add(key, value)
				}
			}
			rw.WriteHeader(cachedResponse.StatusCode)
			_, _ = rw.Write(cachedResponse.Body)
			return
		}
		//log.Printf("Failed to serialize response for caching: %s", err.Error())
		_ = respClient.Delete(req.Context(), cacheKey)
	}

	// Cache miss - record the response
	recorder := &responseRecorder{rw: rw}
	next.ServeHTTP(recorder, req)
	// Serialize the response data
	cachedResponse := CachedResponse{
		StatusCode: recorder.status,
		Headers:    recorder.Header().Clone(), // Convert http.Header to a map for serialization
		Body:       recorder.body.Bytes(),
	}
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	if err := enc.Encode(cachedResponse); err != nil {
		log.Printf("Failed to serialize response for caching: %s", err)
	}

	// Store the serialized response in Redis as a string with an expiration time
	err = respClient.SetWithTTL(req.Context(), cacheKey, buffer.String(), c.cacheExpiry)

	//
	// Write the original response
	rw.WriteHeader(recorder.status)
	_, _ = rw.Write(recorder.body.Bytes())
	return
}
