// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"sync"
	"time"
)

type RateControllerBucketsOptions struct {
	IncreaseTimeThreshold time.Duration
	DecreaseTimeThreshold time.Duration
	IncreaseBitrateChange int
	DecreaseBitrateChange int
}

type rateControllerBuckets struct {
	now                  now
	initialTargetBitrate int
	minBitrate           int
	maxBitrate           int

	dsWriter func(DelayStats)

	lock               sync.Mutex
	init               bool
	delayStats         DelayStats
	target             int
	lastUpdate         time.Time
	lastState          state
	latestRTT          time.Duration
	latestReceivedRate int

	lastIncrease          time.Time
	lastDecrease          time.Time
	rateControllerOptions *RateControllerBucketsOptions

	bitrateControlBucketsManager *Manager
	currentBucketStatus          string
	lastBucketUpdateBitrate      uint64
}

func newRateControllerBuckets(now now, initialTargetBitrate, minBitrate, maxBitrate int, rateControllerOptions *RateControllerBucketsOptions, bitrateControlBucketsManager *Manager, dsw func(DelayStats)) *rateControllerBuckets {
	if rateControllerOptions == nil {
		defaultOptions := RateControllerBucketsOptions{
			IncreaseTimeThreshold: 100 * time.Millisecond,
			DecreaseTimeThreshold: 100 * time.Millisecond,
			IncreaseBitrateChange: 250000,
			DecreaseBitrateChange: 250000,
		}
		rateControllerOptions = &defaultOptions
	}

	return &rateControllerBuckets{
		now:                          now,
		initialTargetBitrate:         initialTargetBitrate,
		minBitrate:                   minBitrate,
		maxBitrate:                   maxBitrate,
		dsWriter:                     dsw,
		init:                         false,
		delayStats:                   DelayStats{},
		target:                       initialTargetBitrate,
		lastUpdate:                   time.Time{},
		lastState:                    stateIncrease,
		latestRTT:                    0,
		latestReceivedRate:           0,
		bitrateControlBucketsManager: bitrateControlBucketsManager,
		currentBucketStatus:          "",
		lastBucketUpdateBitrate:      uint64(initialTargetBitrate),

		rateControllerOptions: rateControllerOptions,
		lastIncrease:          time.Time{},
		lastDecrease:          time.Time{},
	}
}

func (c *rateControllerBuckets) onReceivedRate(rate int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.latestReceivedRate = rate
}

func (c *rateControllerBuckets) updateRTT(rtt time.Duration) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.latestRTT = rtt
}

func (c *rateControllerBuckets) updateBitrate(bitrate int) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.target = minInt(bitrate, c.target)
}

func (c *rateControllerBuckets) onDelayStats(ds DelayStats) {
	now := time.Now()

	if !c.init {
		c.delayStats = ds
		c.delayStats.State = stateIncrease
		c.init = true
		return
	}
	c.delayStats = ds
	c.delayStats.State = c.delayStats.State.transition(ds.Usage)

	if c.delayStats.State == stateHold {
		return
	}

	var next DelayStats

	c.lock.Lock()

	c.currentBucketStatus = ""

	switch c.delayStats.State {
	case stateHold:
		// should never occur due to check above, but makes the linter happy
	case stateIncrease:
		suggestedTarget := clampInt(c.increase(now), c.minBitrate, c.maxBitrate)
		err := c.bitrateControlBucketsManager.CanIncreaseToBitrate(uint64(c.target), uint64(suggestedTarget))
		if err == nil {
			c.target = suggestedTarget
		} else {
			c.currentBucketStatus = err.Error()
		}

		next = DelayStats{
			Measurement:      c.delayStats.Measurement,
			Estimate:         c.delayStats.Estimate,
			Threshold:        c.delayStats.Threshold,
			LastReceiveDelta: c.delayStats.LastReceiveDelta,
			Usage:            c.delayStats.Usage,
			State:            c.delayStats.State,
			TargetBitrate:    c.target,
			ReceivedBitrate:  c.latestReceivedRate,
			LatestRTT:        c.latestRTT,
			BucketStatus:     c.currentBucketStatus,
		}

	case stateDecrease:
		c.target = clampInt(c.decrease(now), c.minBitrate, c.maxBitrate)

		next = DelayStats{
			Measurement:      c.delayStats.Measurement,
			Estimate:         c.delayStats.Estimate,
			Threshold:        c.delayStats.Threshold,
			LastReceiveDelta: c.delayStats.LastReceiveDelta,
			Usage:            c.delayStats.Usage,
			State:            c.delayStats.State,
			TargetBitrate:    c.target,
			ReceivedBitrate:  c.latestReceivedRate,
			LatestRTT:        c.latestRTT,
			BucketStatus:     c.currentBucketStatus,
		}
	}

	c.lock.Unlock()

	c.dsWriter(next)
}

func (c *rateControllerBuckets) increase(now time.Time) int {
	rate := c.target
	if time.Since(c.lastIncrease) > c.rateControllerOptions.IncreaseTimeThreshold {
		c.lastIncrease = time.Now()
		rate = c.target + c.rateControllerOptions.IncreaseBitrateChange
	}
	return rate
}

func (c *rateControllerBuckets) decrease(now time.Time) int {
	rate := c.target
	if time.Since(c.lastDecrease) > c.rateControllerOptions.DecreaseTimeThreshold {
		c.lastIncrease = time.Now()
		rate = c.target - c.rateControllerOptions.DecreaseBitrateChange
	}
	return rate
}
