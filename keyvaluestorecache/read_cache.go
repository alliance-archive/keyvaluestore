package keyvaluestorecache

import (
	"encoding/binary"
	"math"
	"sync"

	"github.com/theaaf/keyvaluestore"
)

// Read cache caches reads permanently, or until they're invalidated by a write operation on the
// cache.
type ReadCache struct {
	backend keyvaluestore.Backend
	cache   *sync.Map

	eventuallyConsistentCache *sync.Map
	eventuallyConsistentReads bool
}

var _ keyvaluestore.Backend = &ReadCache{}

func NewReadCache(b keyvaluestore.Backend) *ReadCache {
	return &ReadCache{
		backend:                   b,
		cache:                     &sync.Map{},
		eventuallyConsistentCache: &sync.Map{},
	}
}

// Returns a new ReadCache that shares the receiver's underlying cache.
func (c *ReadCache) WithBackend(b keyvaluestore.Backend) *ReadCache {
	ret := *c
	ret.backend = b
	return &ret
}

// Returns a new ReadCache suitable for eventually consistent reads. Reads on the returned cache
// will not impact the reads of ancestors with strong consistency. Additionally, the cache will take
// advantage of the fact that items that would have been invalidated by writes may still be returned
// for eventually consistent reads.
func (c *ReadCache) WithEventuallyConsistentReads() *ReadCache {
	if c.eventuallyConsistentReads {
		return c
	}
	ret := *c
	ret.eventuallyConsistentReads = true
	return &ret
}

func (c *ReadCache) load(key string) (interface{}, bool) {
	if c.eventuallyConsistentReads {
		return c.eventuallyConsistentCache.Load(key)
	}
	return c.cache.Load(key)
}

func (c *ReadCache) store(key string, value interface{}) {
	if c.eventuallyConsistentReads {
		c.eventuallyConsistentCache.Store(key, value)
	} else {
		c.cache.Store(key, value)
	}
}

func (c *ReadCache) AtomicWrite() keyvaluestore.AtomicWriteOperation {
	return &readCacheAtomicWriteOperation{
		ReadCache:   c,
		atomicWrite: c.backend.AtomicWrite(),
	}
}

func (c *ReadCache) Batch() keyvaluestore.BatchOperation {
	return &readCacheBatchOperation{
		ReadCache: c,
		batch:     c.backend.Batch(),
	}
}

func (c *ReadCache) Delete(key string) (success bool, err error) {
	success, err = c.backend.Delete(key)
	c.Invalidate(key)
	return success, err
}

type readCacheGetEntry struct {
	value *string
	err   error
}

func (c *ReadCache) Get(key string) (*string, error) {
	v, _ := c.load(key)
	entry, ok := v.(readCacheGetEntry)
	if !ok {
		entry.value, entry.err = c.backend.Get(key)
		c.store(key, entry)
	}
	return entry.value, entry.err
}

func (c *ReadCache) Set(key string, value interface{}) error {
	err := c.backend.Set(key, value)
	c.Invalidate(key)
	return err
}

func (c *ReadCache) CAS(key string, transform func(v *string) (interface{}, error)) (success bool, err error) {
	success, err = c.backend.CAS(key, transform)
	c.Invalidate(key)
	return success, err
}

func (c *ReadCache) AddInt(key string, n int64) (int64, error) {
	n, err := c.backend.AddInt(key, n)
	c.Invalidate(key)
	return n, err
}

func (c *ReadCache) SetXX(key string, value interface{}) (bool, error) {
	ok, err := c.backend.SetXX(key, value)
	c.Invalidate(key)
	return ok, err
}

func (c *ReadCache) SetNX(key string, value interface{}) (bool, error) {
	ok, err := c.backend.SetNX(key, value)
	c.Invalidate(key)
	return ok, err
}

func (c *ReadCache) SAdd(key string, member interface{}, members ...interface{}) error {
	err := c.backend.SAdd(key, member, members...)
	c.Invalidate(key)
	return err
}

func (c *ReadCache) SRem(key string, member interface{}, members ...interface{}) error {
	err := c.backend.SRem(key, member, members...)
	c.Invalidate(key)
	return err
}

type readCacheSMembersEntry struct {
	members []string
	err     error
}

func (c *ReadCache) SMembers(key string) ([]string, error) {
	v, _ := c.load(key)
	entry, ok := v.(readCacheSMembersEntry)
	if !ok {
		entry.members, entry.err = c.backend.SMembers(key)
		c.store(key, entry)
	}
	return entry.members, entry.err
}

func (c *ReadCache) ZAdd(key string, member interface{}, score float64) error {
	err := c.backend.ZAdd(key, member, score)
	c.Invalidate(key)
	return err
}

type readCacheZScoreEntry struct {
	score *float64
	err   error
}

func (c *ReadCache) ZScore(key string, member interface{}) (*float64, error) {
	s := *keyvaluestore.ToString(member)
	subkey := concatKeys("zs", s)
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZScoreEntry); ok {
			return entry.score, entry.err
		}
	}
	score, err := c.backend.ZScore(key, member)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}
	zEntry.subcache[subkey] = readCacheZScoreEntry{
		score: score,
		err:   err,
	}
	c.store(key, zEntry)
	return score, err
}

func (c *ReadCache) ZIncrBy(key string, member string, n float64) (float64, error) {
	val, err := c.backend.ZIncrBy(key, member, n)
	c.Invalidate(key)
	return val, err
}

func (c *ReadCache) ZRem(key string, member interface{}) error {
	err := c.backend.ZRem(key, member)
	c.Invalidate(key)
	return err
}

type readCacheZEntry struct {
	subcache map[string]interface{}
}

type readCacheZCountEntry struct {
	count int
	err   error
}

func (c *ReadCache) ZCount(key string, min, max float64) (int, error) {
	subkey := concatKeys("zc", floatKey(min), floatKey(max))
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZCountEntry); ok {
			return entry.count, entry.err
		}
	}
	count, err := c.backend.ZCount(key, min, max)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}
	zEntry.subcache[subkey] = readCacheZCountEntry{
		count: count,
		err:   err,
	}
	c.store(key, zEntry)
	return count, err
}

func (c *ReadCache) ZLexCount(key string, min, max string) (int, error) {
	subkey := concatKeys("zlc", min, max)
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZCountEntry); ok {
			return entry.count, entry.err
		}
	}
	count, err := c.backend.ZLexCount(key, min, max)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}
	zEntry.subcache[subkey] = readCacheZCountEntry{
		count: count,
		err:   err,
	}
	c.store(key, zEntry)
	return count, err
}

type readCacheZRangeEntry struct {
	members keyvaluestore.ScoredMembers
	limit   int
	err     error
}

func floatKey(f float64) string {
	n := math.Float64bits(f)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, n)
	return string(buf)
}

func (c *ReadCache) ZRangeByScore(key string, min, max float64, limit int) ([]string, error) {
	members, err := c.ZRangeByScoreWithScores(key, min, max, limit)
	return members.Values(), err
}

func (c *ReadCache) ZRangeByScoreWithScores(key string, min, max float64, limit int) (keyvaluestore.ScoredMembers, error) {
	subkey := concatKeys("zrbs", floatKey(min), floatKey(max))
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZRangeEntry); ok && limit <= entry.limit {
			return entry.members, entry.err
		}
	}
	members, err := c.backend.ZRangeByScoreWithScores(key, min, max, limit)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}
	zEntry.subcache[subkey] = readCacheZRangeEntry{
		members: members,
		limit:   limit,
		err:     err,
	}
	c.store(key, zEntry)
	return members, err
}

func (c *ReadCache) ZRevRangeByScore(key string, min, max float64, limit int) ([]string, error) {
	members, err := c.ZRevRangeByScoreWithScores(key, min, max, limit)
	return members.Values(), err
}

func (c *ReadCache) ZRevRangeByScoreWithScores(key string, min, max float64, limit int) (keyvaluestore.ScoredMembers, error) {
	subkey := concatKeys("zrrbs", floatKey(min), floatKey(max))
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZRangeEntry); ok && limit <= entry.limit {
			return entry.members, entry.err
		}
	}
	members, err := c.backend.ZRevRangeByScoreWithScores(key, min, max, limit)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}
	zEntry.subcache[subkey] = readCacheZRangeEntry{
		members: members,
		limit:   limit,
		err:     err,
	}
	c.store(key, zEntry)
	return members, err
}

func (c *ReadCache) ZRangeByLex(key string, min, max string, limit int) ([]string, error) {
	subkey := concatKeys("zrbl", min, max)
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZRangeEntry); ok && limit <= entry.limit {
			return entry.members.Values(), entry.err
		}
	}
	members, err := c.backend.ZRangeByLex(key, min, max, limit)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}

	scoredMembers := make([]*keyvaluestore.ScoredMember, len(members))

	for i, member := range members {
		scoredMembers[i] = &keyvaluestore.ScoredMember{Value: member}
	}

	zEntry.subcache[subkey] = readCacheZRangeEntry{
		members: scoredMembers,
		limit:   limit,
		err:     err,
	}
	c.store(key, zEntry)
	return members, err
}

func (c *ReadCache) ZRevRangeByLex(key string, min, max string, limit int) ([]string, error) {
	subkey := concatKeys("zrrbl", min, max)
	v, _ := c.load(key)
	zEntry, ok := v.(readCacheZEntry)
	if ok {
		if entry, ok := zEntry.subcache[subkey].(readCacheZRangeEntry); ok && limit <= entry.limit {
			return entry.members.Values(), entry.err
		}
	}
	members, err := c.backend.ZRevRangeByLex(key, min, max, limit)
	if zEntry.subcache == nil {
		zEntry.subcache = make(map[string]interface{})
	}

	scoredMembers := make([]*keyvaluestore.ScoredMember, len(members))

	for i, member := range members {
		scoredMembers[i] = &keyvaluestore.ScoredMember{Value: member}
	}

	zEntry.subcache[subkey] = readCacheZRangeEntry{
		members: scoredMembers,
		limit:   limit,
		err:     err,
	}
	c.store(key, zEntry)
	return members, err
}

func (c *ReadCache) Invalidate(key string) {
	c.cache.Delete(key)
}

func concatKeys(s ...string) string {
	l := 0
	for _, s := range s {
		l += 8 + len(s)
	}
	ret := make([]byte, l)
	dest := ret
	for _, s := range s {
		binary.BigEndian.PutUint64(dest, uint64(len(s)))
		if len(s) > 0 {
			copy(dest[8:], s)
		}
		dest = dest[8+len(s):]
	}
	return string(ret)
}
