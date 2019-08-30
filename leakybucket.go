package limiters

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	"github.com/go-redis/redis"
	"github.com/pkg/errors"
)

// LeakyBucketState represents the state of a LeakyBucket.
type LeakyBucketState struct {
	// Last is the Unix timestamp in nanoseconds of the most recent request.
	Last int64
}

// IzZero returns true if the bucket state is zero valued.
func (s LeakyBucketState) IzZero() bool {
	return s.Last == 0
}

// LeakyBucketStateBackend interface encapsulates the logic of retrieving and persisting the state of a LeakyBucket.
type LeakyBucketStateBackend interface {
	// State gets the current state of the LeakyBucket.
	State(ctx context.Context) (LeakyBucketState, error)
	// SetState sets (persists) the current state of the LeakyBucket.
	SetState(ctx context.Context, state LeakyBucketState, fencingToken int64) error
}

// LeakyBucket implements the https://en.wikipedia.org/wiki/Leaky_bucket#As_a_queue algorithm.
type LeakyBucket struct {
	Locker
	LeakyBucketStateBackend
	Clock
	Logger
	// Capacity is the maximum allowed number of tockens in the bucket.
	Capacity int64
	// Rate is the output rate: 1 request per the rate duration (in nanoseconds).
	Rate int64
	mu   sync.Mutex
}

// NewLeakyBucket creates a new instance of LeakyBucket.
func NewLeakyBucket(capacity int64, rate time.Duration, locker Locker, leakyBucketStateBackend LeakyBucketStateBackend, clock Clock, logger Logger) *LeakyBucket {
	return &LeakyBucket{
		Locker:                  locker,
		LeakyBucketStateBackend: leakyBucketStateBackend,
		Clock:                   clock,
		Logger:                  logger,
		Capacity:                capacity,
		Rate:                    int64(rate),
	}
}

// Limit returns the time duration to wait before the request can be processed.
// If the last request happened earlier than the rate this method returns zero duration.
// It returns ErrLimitExhausted if the the request overflows the bucket's capacity. In this case the returned duration
// means how long it would have taken to wait for the request to be processed if the bucket was not overflowed.
func (t *LeakyBucket) Limit(ctx context.Context) (time.Duration, error) {
	now := t.Clock.Now().UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	fencingToken, err := t.Lock(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := t.Unlock(); err != nil {
			t.Logger.Log(err)
		}
	}()
	state, err := t.State(ctx)
	if err != nil {
		return 0, err
	}
	if now < state.Last {
		// The queue has requests in it: move the current request to the last position + 1.
		state.Last += t.Rate
	} else {
		// The queue is empty.
		// The offset is the duration to wait in case the last request happened less than rate duration ago.
		var offset int64
		delta := now - state.Last
		if delta < t.Rate {
			offset = t.Rate - delta
		}
		state.Last = now + offset
	}

	wait := state.Last - now
	if wait/t.Rate > t.Capacity {
		return time.Duration(wait), ErrLimitExhausted
	}
	if err := t.SetState(ctx, state, fencingToken); err != nil {
		return 0, err
	}
	return time.Duration(wait), nil
}

// LeakyBucketInMemory is an in-memory implementation of LeakyBucketStateBackend.
type LeakyBucketInMemory struct {
	state        LeakyBucketState
	fencingToken int64
}

// NewLeakyBucketInMemory creates a new instance of LeakyBucketInMemory.
func NewLeakyBucketInMemory() *LeakyBucketInMemory {
	return &LeakyBucketInMemory{}
}

// State gets the current state of the bucket.
func (l *LeakyBucketInMemory) State(ctx context.Context) (LeakyBucketState, error) {
	return l.state, ctx.Err()
}

// SetState sets the current state of the bucket.
func (l *LeakyBucketInMemory) SetState(ctx context.Context, state LeakyBucketState, fencingToken int64) error {
	if fencingToken < l.fencingToken {
		return ErrFencingTokenExpired
	}
	l.state = state
	l.fencingToken = fencingToken
	return ctx.Err()
}

const (
	etcdKeyLBLease        = "lease"
	etcdKeyLBLast         = "last"
	etcdKeyLBFencingToken = "fencing_token"
)

// LeakyBucketEtcd is an etcd implementation of a LeakyBucketStateBackend.
// See the TokenBucketEtcd description for the details on etcd usage.
type LeakyBucketEtcd struct {
	// prefix is the etcd key prefix.
	prefix  string
	cli     *clientv3.Client
	leaseID clientv3.LeaseID
	ttl     time.Duration
}

// NewLeakyBucketEtcd creates a new LeakyBucketEtcd instance.
// Prefix is used as an etcd key prefix for all keys stored in etcd by this algorithm.
// TTL is a TTL of the etcd lease used to store all the keys.
func NewLeakyBucketEtcd(cli *clientv3.Client, prefix string, ttl time.Duration) *LeakyBucketEtcd {
	return &LeakyBucketEtcd{
		prefix: prefix,
		cli:    cli,
		ttl:    ttl,
	}
}

// State gets the bucket's current state from etcd.
// If there is no state available in etcd then the initial bucket's state is returned.
func (l *LeakyBucketEtcd) State(ctx context.Context) (LeakyBucketState, error) {
	// Reset the lease ID as it will be updated by the successful Get operation below.
	l.leaseID = 0
	// Get all the keys under the prefix in a single request.
	r, err := l.cli.Get(ctx, l.prefix, clientv3.WithRange(incPrefix(l.prefix)))
	if err != nil {
		return LeakyBucketState{}, errors.Wrapf(err, "failed to get keys in range ['%s', '%s') from etcd", l.prefix, incPrefix(l.prefix))
	}
	if len(r.Kvs) == 0 {
		return LeakyBucketState{}, nil
	}
	state := LeakyBucketState{}
	parsed := 0
	for _, kv := range r.Kvs {
		switch string(kv.Key) {
		case etcdKey(l.prefix, etcdKeyLBLast):
			v, err := parseEtcdInt64(kv)
			if err != nil {
				return LeakyBucketState{}, err
			}
			state.Last = v
			parsed |= 1

		case etcdKey(l.prefix, etcdKeyLBLease):
			v, err := parseEtcdInt64(kv)
			if err != nil {
				return LeakyBucketState{}, err
			}
			l.leaseID = clientv3.LeaseID(v)
			parsed |= 2
		}
	}
	if parsed != 3 {
		return LeakyBucketState{}, errors.New("failed to get state from etcd: some keys are missing")
	}
	return state, nil
}

// createLease creates a new lease in etcd and updates the t.leaseID value.
func (l *LeakyBucketEtcd) createLease(ctx context.Context) error {
	lease, err := l.cli.Grant(ctx, int64(l.ttl/time.Nanosecond))
	if err != nil {
		return errors.Wrap(err, "failed to create a new lease in etcd")
	}
	l.leaseID = lease.ID
	return nil
}

// save saves the state to etcd using the existing lease.
func (l *LeakyBucketEtcd) save(ctx context.Context, state LeakyBucketState, fencingToken int64) error {
	r, err := l.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.CreateRevision(etcdKey(l.prefix, etcdKeyLBFencingToken)), "!=", 0),
		clientv3.Compare(clientv3.Value(etcdKey(l.prefix, etcdKeyLBFencingToken)), ">", fmt.Sprintf("%d", fencingToken)),
	).Else(
		clientv3.OpPut(etcdKey(l.prefix, etcdKeyLBLast), fmt.Sprintf("%d", state.Last), clientv3.WithLease(l.leaseID)),
		clientv3.OpPut(etcdKey(l.prefix, etcdKeyLBLease), fmt.Sprintf("%d", l.leaseID), clientv3.WithLease(l.leaseID)),
		clientv3.OpPut(etcdKey(l.prefix, etcdKeyLBFencingToken), fmt.Sprintf("%d", fencingToken), clientv3.WithLease(l.leaseID)),
	).Commit()
	if err != nil {
		return errors.Wrap(err, "failed to commit a transaction to etcd")
	}

	if !r.Succeeded {
		return nil
	}
	return ErrFencingTokenExpired
}

// SetState updates the state of the bucket in etcd.
func (l *LeakyBucketEtcd) SetState(ctx context.Context, state LeakyBucketState, fencingToken int64) error {
	if l.leaseID == 0 {
		// Lease does not exist, create one.
		if err := l.createLease(ctx); err != nil {
			return err
		}
		// No need to send KeepAlive for the newly creates lease: save the state immediately.
		return l.save(ctx, state, fencingToken)
	}
	// Send the KeepAlive request to extend the existing lease.
	if _, err := l.cli.KeepAliveOnce(ctx, l.leaseID); err == rpctypes.ErrLeaseNotFound {
		// Create a new lease since the current one has expired.
		if err := l.createLease(ctx); err != nil {
			return err
		}
	} else if err != nil {
		return errors.Wrapf(err, "failed to extend the lease '%d'", l.leaseID)
	}

	return l.save(ctx, state, fencingToken)
}

const (
	redisKeyLBLast         = "last"
	redisKeyLBFencingToken = "fencing_token"
)

// LeakyBucketRedis is a Redis implementation of a LeakyBucketStateBackend.
type LeakyBucketRedis struct {
	cli          *redis.Client
	prefix       string
	ttl          time.Duration
	fencingToken int64
}

// NewLeakyBucketRedis creates a new LeakyBucketRedis instance.
// Prefix is the key prefix used to store all the keys used in this implementation in Redis.
// TTL is the TTL of the stored keys.
func NewLeakyBucketRedis(cli *redis.Client, prefix string, ttl time.Duration) *LeakyBucketRedis {
	return &LeakyBucketRedis{cli: cli, prefix: prefix, ttl: ttl}
}

// State gets the bucket's state from Redis.
func (t *LeakyBucketRedis) State(ctx context.Context) (LeakyBucketState, error) {
	var values []interface{}
	var err error
	done := make(chan struct{}, 1)
	go func() {
		values, err = t.cli.MGet(
			redisKey(t.prefix, redisKeyLBLast),
			redisKey(t.prefix, redisKeyLBFencingToken),
		).Result()
		done <- struct{}{}
	}()

	select {
	case <-done:

	case <-ctx.Done():
		return LeakyBucketState{}, ctx.Err()
	}

	if err != nil {
		return LeakyBucketState{}, errors.Wrap(err, "failed to get keys from redis")
	}
	nilAny := false
	for _, v := range values {
		if v == nil {
			nilAny = true
			break
		}
	}
	if nilAny || err == redis.Nil {
		// Keys don't exist, return an empty state.
		return LeakyBucketState{}, nil
	}

	last, err := strconv.ParseInt(values[0].(string), 10, 64)
	if err != nil {
		return LeakyBucketState{}, err
	}
	t.fencingToken, err = strconv.ParseInt(values[1].(string), 10, 64)
	if err != nil {
		return LeakyBucketState{}, err
	}
	return LeakyBucketState{
		Last: last,
	}, nil
}

// checkResponseFromRedis checks that all responses from Redis are "OK".
func checkResponseFromRedis(response interface{}, count int) error {
	if s, sok := response.(string); sok && s == "TOKEN_EXPIRED" {
		return ErrFencingTokenExpired
	}
	r, ok := response.([]interface{})
	if !ok {
		return errors.Errorf("unexpected response format from redis: %T %+v", response, response)
	}
	if len(r) != count {
		return errors.Errorf("got %d response items from redis, expected %d", len(r), count)
	}
	for _, w := range r {
		m, ok := w.(string)
		if !ok {
			return errors.Errorf("unexpected response from redis: %+v", w)
		}
		if m != "OK" {
			return errors.Errorf("got '%s' from redis, expected 'OK'", w)
		}
	}
	return nil
}

// SetState updates the state in Redis.
// The provided fencing token is checked on the Redis side before saving the keys.
func (t *LeakyBucketRedis) SetState(ctx context.Context, state LeakyBucketState, fencingToken int64) error {
	var result interface{}
	var err error
	done := make(chan struct{}, 1)
	go func() {
		result, err = t.cli.Eval(`
	local token = tonumber(redis.call('get', KEYS[1])) or 0
	if token <= tonumber(ARGV[1]) then
		return {
			redis.call('set', KEYS[1], ARGV[1], 'PX', ARGV[3]),
			redis.call('set', KEYS[2], ARGV[2], 'PX', ARGV[3]),
		}
	else
		return 'TOKEN_EXPIRED'
	end
	`, []string{
			redisKey(t.prefix, redisKeyLBFencingToken),
			redisKey(t.prefix, redisKeyLBLast),
		},
			fencingToken,
			state.Last,
			// TTL in milliseconds.
			int64(t.ttl/time.Microsecond)).Result()
		done <- struct{}{}
	}()

	select {
	case <-done:

	case <-ctx.Done():
		return ctx.Err()
	}

	if err != nil {
		return errors.Wrap(err, "failed to save keys to redis")
	}
	return checkResponseFromRedis(result, 2)
}
