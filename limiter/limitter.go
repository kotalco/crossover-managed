package limiter

import (
	"context"
	"errors"
	"fmt"
	"github.com/kotalco/resp"
	"net/http"
	"strconv"
)

const (
	UserRateKeySuffix      = "-rate"
	UserRateLimitingWindow = 1 //sec
)

type ILimiter interface {
	Limit(ctx context.Context, userId string, respClint resp.IClient) (bool, error)
}
type limiter struct {
	planProxy IPlanProxy
}

func NewLimiter(apiKey string, remoteAddress string) ILimiter {
	return &limiter{
		planProxy: NewPlanProxy(apiKey, remoteAddress),
	}
}

func (l *limiter) Limit(ctx context.Context, userId string, respClint resp.IClient) (bool, error) {

	userPlan, err := l.getUserPlan(ctx, respClint, userId)
	if err != nil {
		return false, err
	}

	allow, err := l.allow(ctx, respClint, userId, userPlan)
	if err != nil {
		return false, err
	}
	if !allow {
		return false, errors.New(http.StatusText(http.StatusTooManyRequests))
	}
	return true, nil
}

func (l *limiter) getUserPlan(ctx context.Context, respClint resp.IClient, userId string) (int, error) {
	//get user plan from cache
	userPlan, err := respClint.Get(ctx, userId)
	if err != nil {
		return 0, err
	}

	//fetch user plan from proxy if it doesn't exist
	if userPlan == "" {
		userPlan, err = l.planProxy.fetch(userId)
		if err != nil {
			return 0, err
		}
		//set user plan to cache
		err = respClint.Set(ctx, userId, userPlan)
		if err != nil {
			return 0, err
		}
	}

	//parse user plan to int
	userPlanInt, err := strconv.Atoi(userPlan)
	if err != nil {
		return 0, errors.New(fmt.Sprintf("can't parse userPlan: %s, got error: %s", userPlan, err.Error()))
	}
	return userPlanInt, nil
}

func (l *limiter) allow(ctx context.Context, respClint resp.IClient, userId string, limit int) (bool, error) {
	//user limiting cache key
	key := fmt.Sprintf("%s%s", userId, UserRateKeySuffix)

	// Increment the counter for the given key.
	count, err := respClint.Incr(ctx, key)
	if err != nil {
		return false, err
	}
	if count == 1 {
		// If the key is new or expired (i.e., count == 1), set the expiration.
		_, err = respClint.Expire(ctx, key, UserRateLimitingWindow)
		if err != nil {
			return false, err
		}
	}

	// Check against the limit.
	return count <= limit, nil
}
