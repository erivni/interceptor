// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"math"
	"sync"
	"time"

	"github.com/pion/interceptor/internal/cc"
	"github.com/pion/logging"
)

// LossStats contains internal statistics of the loss based controller
type LossStats struct {
	TargetBitrate int
	AverageLoss   float64
	BucketStatus  string
}

type lossBasedBandwidthEstimator struct {
	lock                         sync.Mutex
	maxBitrate                   int
	minBitrate                   int
	bitrate                      int
	currentBucketStatus          string
	lastBucketUpdateBitrate      uint64
	averageLoss                  float64
	lastLossUpdate               time.Time
	lastIncrease                 time.Time
	lastDecrease                 time.Time
	options                      LossBasedBandwidthEstimatorOptions
	bitrateControlBucketsManager *Manager
	log                          logging.LeveledLogger
}

type LossBasedBandwidthEstimatorOptions struct {
	IncreaseLossThreshold float64
	IncreaseTimeThreshold time.Duration
	IncreaseFactor        float64
	DecreaseLossThreshold float64
	DecreaseTimeThreshold time.Duration
	DecreaseFactor        float64
}

func newLossBasedBWE(initialBitrate int, minBitrate int, maxBitrate int, options *LossBasedBandwidthEstimatorOptions, bitrateControlBucketsManager *Manager) *lossBasedBandwidthEstimator {
	if options == nil {
		// constants from
		// https://datatracker.ietf.org/doc/html/draft-ietf-rmcat-gcc-02#section-6
		defaultOptions := LossBasedBandwidthEstimatorOptions{
			IncreaseLossThreshold: 0.02,
			IncreaseTimeThreshold: 200 * time.Millisecond,
			IncreaseFactor:        1.05,
			DecreaseLossThreshold: 0.1,
			DecreaseTimeThreshold: 200 * time.Millisecond,
			DecreaseFactor:        1.0,
		}
		options = &defaultOptions
	}

	return &lossBasedBandwidthEstimator{
		lock:                         sync.Mutex{},
		maxBitrate:                   maxBitrate,
		minBitrate:                   minBitrate,
		bitrate:                      initialBitrate,
		currentBucketStatus:          "",
		lastBucketUpdateBitrate:      uint64(initialBitrate),
		averageLoss:                  0,
		lastLossUpdate:               time.Time{},
		lastIncrease:                 time.Time{},
		lastDecrease:                 time.Time{},
		options:                      *options,
		bitrateControlBucketsManager: bitrateControlBucketsManager,
		log:                          logging.NewDefaultLoggerFactory().NewLogger("gcc_loss_controller"),
	}
}

func (e *lossBasedBandwidthEstimator) getEstimate(wantedRate int) LossStats {
	e.lock.Lock()
	defer e.lock.Unlock()

	if e.bitrate <= 0 {
		e.bitrate = clampInt(wantedRate, e.minBitrate, e.maxBitrate)
	}

	e.bitrate = minInt(wantedRate, e.bitrate)

	latestBitrate, _ := e.bitrateControlBucketsManager.getBucket(uint64(e.bitrate))

	return LossStats{
		TargetBitrate: int(latestBitrate),
		AverageLoss:   e.averageLoss,
		BucketStatus:  e.currentBucketStatus,
	}
}

func (e *lossBasedBandwidthEstimator) updateLossEstimate(results []cc.Acknowledgment) {
	if len(results) == 0 {
		return
	}

	packetsLost := 0
	for _, p := range results {
		if p.Arrival.IsZero() {
			packetsLost++
		}
	}

	e.lock.Lock()
	defer e.lock.Unlock()

	lossRatio := float64(packetsLost) / float64(len(results))
	e.averageLoss = e.average(time.Since(e.lastLossUpdate), e.averageLoss, lossRatio)
	e.lastLossUpdate = time.Now()

	increaseLoss := math.Max(e.averageLoss, lossRatio)
	decreaseLoss := math.Min(e.averageLoss, lossRatio)

	e.currentBucketStatus = ""

	if increaseLoss < e.options.IncreaseLossThreshold {
		if time.Since(e.lastIncrease) > e.options.IncreaseTimeThreshold {
			e.log.Infof("loss controller increasing; averageLoss: %v, decreaseLoss: %v, increaseLoss: %v, currentBitrate: %v", e.averageLoss, decreaseLoss, increaseLoss, e.bitrate)
			suggestedTarget := clampInt(int(e.options.IncreaseFactor*float64(e.bitrate)), e.minBitrate, e.maxBitrate)
			currentBitrateBucket, _ := e.bitrateControlBucketsManager.getBucket(uint64(e.bitrate))
			newBitrateBucket, _ := e.bitrateControlBucketsManager.getBucket(uint64(suggestedTarget))
			if currentBitrateBucket != newBitrateBucket {
				err := e.bitrateControlBucketsManager.CanIncreaseToBitrate(currentBitrateBucket, newBitrateBucket)
				if err == nil {
					e.lastIncrease = time.Now()
					e.bitrate = suggestedTarget
				} else {
					e.currentBucketStatus = err.Error()
				}
			} else {
				e.lastIncrease = time.Now()
				e.bitrate = suggestedTarget
			}
		}
	} else if decreaseLoss > e.options.DecreaseLossThreshold {
		if time.Since(e.lastDecrease) > e.options.DecreaseTimeThreshold {
			e.log.Infof("loss controller decreasing; averageLoss: %v, decreaseLoss: %v, increaseLoss: %v, currentBitrate: %v", e.averageLoss, decreaseLoss, increaseLoss, e.bitrate)
			e.lastDecrease = time.Now()
			e.bitrate = clampInt(int(float64(e.bitrate)*(1.0-e.options.DecreaseFactor*decreaseLoss)), e.minBitrate, e.maxBitrate)
		}
	}
}

func (e *lossBasedBandwidthEstimator) average(delta time.Duration, prev, sample float64) float64 {
	return sample + math.Exp(-float64(delta.Milliseconds())/200.0)*(prev-sample)
}
