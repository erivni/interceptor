package gcc

import (
	"fmt"
	"sort"
	"time"

	//"github.com/pion/logging"
)

type BitrateControlBucketsConfig struct {
	BitrateStableThreshold              uint32
	HandleUnstableBitrateGracePeriodSec uint32
	BitrateBucketIncrement              uint64
	BackoffDurationsSec                 []float64
}

type bucket struct {
	config *BitrateControlBucketsConfig

	backoffDurationSec float64
	stable             bool
	stabilizationCount uint32
	blockedUntil       time.Time
	boundaries         string

	backoffDurationsIndex int
}

type Manager struct {
	config              *BitrateControlBucketsConfig
	buckets             map[uint64]*bucket
	bucketsKeys         []uint64
	bucketIncrement     uint64
	lastUnstableBitrate time.Time
	//log                 logging.LeveledLogger
}

func NewManager(config *BitrateControlBucketsConfig) *Manager {
	return &Manager{
		config:              config,
		buckets:             make(map[uint64]*bucket),
		bucketIncrement:     config.BitrateBucketIncrement,
		lastUnstableBitrate: time.Time{},
	}
}
func (manager *Manager) InitializeBuckets(maxBitrateKbps uint64) {
	bucketIncrement := manager.config.BitrateBucketIncrement
	if manager.bucketIncrement != bucketIncrement {
		manager.buckets = make(map[uint64]*bucket)
		manager.bucketIncrement = bucketIncrement
	}
	var prevBucket *bucket = nil
	/*
		Each bucket has an upper limit x and a lower limit y
		the bucket include every bitrate when y<bitrate<=x
		The bucket index is the upper limit
		Example: bucket 500 covers bitrates 0-500
				bucket 1000 covers bitrates 501-1000
	*/
	i := bucketIncrement
	for ; i <= maxBitrateKbps; i += bucketIncrement {
		// Create only bitrate buckets which do not exist
		if _, exist := manager.buckets[i]; !exist {
			manager.buckets[i] = newBucket(manager.config, prevBucket, i-bucketIncrement, i)
		}
		prevBucket = manager.buckets[i]
	}
	if maxBitrateKbps%bucketIncrement != 0 {
		manager.buckets[maxBitrateKbps] = newBucket(manager.config, prevBucket, i-bucketIncrement, maxBitrateKbps)
	}
	manager.bucketsKeys = manager.getBucketSortedKeys()
}

func newBucket(config *BitrateControlBucketsConfig, prevBucket *bucket, lowerLimit uint64, upperLimit uint64) *bucket {
	bucketBoundaries := fmt.Sprintf("(%d-%d)", lowerLimit, upperLimit)
	bucket := &bucket{
		config:     config,
		boundaries: bucketBoundaries,
	}
	bucket.setStable()
	// If a lower bitrate bucket exist, use it's data
	if prevBucket != nil {
		bucket.backoffDurationSec = prevBucket.backoffDurationSec
		bucket.stable = prevBucket.stable
		bucket.stabilizationCount = prevBucket.stabilizationCount
		bucket.blockedUntil = prevBucket.blockedUntil
		bucket.backoffDurationsIndex = prevBucket.backoffDurationsIndex
	}
	return bucket
}

func (manager *Manager) getBucketSortedKeys() []uint64 {
	var keys []uint64
	for key := range manager.buckets {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (manager *Manager) GetBackoffSec(bitrateKbps uint64) float64 {
	_, bucket := manager.getBucket(bitrateKbps)
	if bucket != nil {
		return bucket.backoffDurationSec
	}
	return 0
}

func (manager *Manager) getBucket(bitrateKbps uint64) (uint64, *bucket) {
	for _, key := range manager.bucketsKeys {
		if key >= bitrateKbps {
			return key, manager.buckets[key]
		}
	}
	return 0, nil
}

func (manager *Manager) HandleBitrateNormal(bitrateKbps uint64) {
	_, bucket := manager.getBucket(bitrateKbps)
	if bucket == nil {
		return
	}
	bucket.stabilizationCount++

	if bucket.stabilizationCount >= manager.config.BitrateStableThreshold {
		manager.markBitrateStable(bitrateKbps)
	}
}

func (manager *Manager) HandleBitrateDecrease(bitrateKbps uint64) {
	gracePeriod := time.Duration(manager.config.HandleUnstableBitrateGracePeriodSec) * time.Second
	if time.Since(manager.lastUnstableBitrate) > gracePeriod {
		manager.lastUnstableBitrate = time.Now()
		manager.markBitrateUnstable(bitrateKbps)
	}
}

func (manager *Manager) CanIncreaseToBitrate(currentBitrate, nextBitrate uint64) error {
	// logger := manager.log.WithFields(log.Fields{
	// 	"subcomponent":   "bitrateControlBuckets",
	// 	"currentBitrate": currentBitrate,
	// 	"nextBitrate":    nextBitrate,
	// })
	_, bucket := manager.getBucket(currentBitrate)
	if bucket == nil {
		err := fmt.Errorf("bucket for current bitrate %d does not exist", currentBitrate)
		//logger.Error(err.Error())
		return err
	}
	if !bucket.stable {
		return fmt.Errorf("current bitrate %d is within bucket %s and is unstable (%d/%d)", currentBitrate, bucket.boundaries, bucket.stabilizationCount, manager.config.BitrateStableThreshold)
	}
	_, bucket = manager.getBucket(nextBitrate)
	if bucket == nil {
		err := fmt.Errorf("bucket for next bitrate %d does not exist", nextBitrate)
		//logger.Error(err.Error())
		return err
	}
	secondsToUnblock := bucket.getSecondsToUnblock()
	if secondsToUnblock >= 0 {
		return fmt.Errorf("next bitrate %d is within bucket %s and blocked for another %fs", nextBitrate, bucket.boundaries, secondsToUnblock)
	}
	return nil
}

func (manager *Manager) markBitrateStable(bitrateKbps uint64) {
	// This bitrate is now considered stable, mark it's bucket and all LOWER bitrate buckets as stable as well
	bucketKey, _ := manager.getBucket(bitrateKbps)
	for _, key := range manager.bucketsKeys {
		if key <= bucketKey {
			manager.buckets[key].setStable()
		}
	}
}

func (manager *Manager) markBitrateUnstable(bitrateKbps uint64) {
	bucketKey, matchedBucket := manager.getBucket(bitrateKbps)
	if matchedBucket == nil {
		return
	}

	matchedBucket.setUnstable()
	matchedBucket.addBackoff()
	// We also mark all higher bitrates buckets as unstable, and increase their block time if needed
	for _, key := range manager.bucketsKeys {
		if key > bucketKey {
			manager.buckets[key].setUnstable()
			manager.buckets[key].setMinBlockTime(matchedBucket.blockedUntil)
		}
	}
}

/* BitrateControlBucket functions */
func (b *bucket) setStable() {
	b.stable = true
	b.stabilizationCount = 0
	b.backoffDurationSec = 0
	b.backoffDurationsIndex = -1 // there is no back off
	b.blockedUntil = time.Now()
}

func (b *bucket) setUnstable() {
	b.stable = false
	b.stabilizationCount = 0
}

func (b *bucket) addBackoff() {
	if len(b.config.BackoffDurationsSec) == 0 {
		return
	}
	if b.backoffDurationsIndex < len(b.config.BackoffDurationsSec)-1 {
		b.backoffDurationsIndex++
		b.backoffDurationSec = b.config.BackoffDurationsSec[b.backoffDurationsIndex]
	}
	b.blockedUntil = time.Now().Add(time.Duration(b.backoffDurationSec) * time.Second)
}

func (b *bucket) setMinBlockTime(endTime time.Time) {
	if b.blockedUntil.Before(endTime) {
		b.blockedUntil = endTime
	}
}

func (b *bucket) getSecondsToUnblock() float64 {
	return time.Until(b.blockedUntil).Seconds()
}
