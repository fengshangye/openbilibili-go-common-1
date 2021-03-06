package service

import (
	"context"
	"io/ioutil"
	"os"
	"time"

	"go-common/app/job/main/passport-game-data/model"
	"go-common/library/log"
)

// local2cloudcompareproc compare aso accounts between local and cloud, delay for the given duration.
// select last modified from cloud
// load batchSize from origin
// compare:
// if cloud_mtime >= local_mtime, directly compare
// if cloud_mtime < local_mtime, sleep and reload from cloud, then do compare again
func (s *Service) local2cloudcompareproc() {
	var (
		err      error
		localRes []*model.OriginAsoAccount

		ack = false
	)

	cc := s.l2cC

	delay := cc.DelayDuration

	cc.st = cc.StartTime
	cc.ed = cc.st.Add(cc.StepDuration)

	offsetFile, err := os.Create(cc.OffsetFilePath)
	if err != nil {
		log.Error("failed to create offset file, os.Create(%s) error(%v)", cc.OffsetFilePath, err)
		return
	}
	defer offsetFile.Close()
	log.Info("created offset file %s", cc.OffsetFilePath)

	for {
		time.Sleep(cc.LoopDuration)

		cc.sleeping = false

		if ack {
			cc.st = cc.st.Add(cc.StepDuration)
			cc.ed = cc.ed.Add(cc.StepDuration)
		}

		st, ed := cc.st, cc.ed

		if err = ioutil.WriteFile(cc.OffsetFilePath, []byte(st.Format(_timeFormat)), os.ModeAppend); err != nil {
			log.Error("failed to write offset, ioutil.WriteFile(%s, %s, os.ModeAppend), error(%v)", cc.OffsetFilePath, st.Format(_timeFormat), err)
			continue
		}

		if cc.Debug {
			log.Info("st: %s, ed: %s", st.Format(_timeFormat), ed.Format(_timeFormat))
		}

		if cc.End && st.After(cc.EndTime) {
			log.Info("st:%s is after endTime:%s, all data compares ok, local2cloudcompareproc exit", st.Format(_timeFormat), cc.EndTime.Format(_timeFormat))
			return
		}

		now := time.Now()

		if now.Sub(st) <= delay {
			delta := int64(delay/time.Second) - (now.Unix() - st.Unix())
			log.Info("now time is just after st by %d seconds, not greater than delay duration: %v, will sleep %d seconds", int64(delay/time.Second)-delta, delay, delta)

			cc.sleeping = true
			cc.sleepingSeconds = delta
			cc.sleepFromTs = now.Unix()

			time.Sleep(time.Duration(int64(time.Second) * delta))
			continue
		}

		if localRes, err = s.d.AsoAccountRangeLocal(context.TODO(), st, ed); err != nil {
			continue
		}
		cc.rangeCount = len(localRes)
		cc.totalCount += len(localRes)

		if err = s.local2CloudCompare(context.TODO(), localRes); err != nil {
			continue
		}

		ack = true
	}
}

func (s *Service) local2CloudCompare(c context.Context, lRes []*model.OriginAsoAccount) (err error) {
	mids := make([]int64, 0)
	for _, item := range lRes {
		mids = append(mids, item.Mid)
	}

	cc := s.l2cC
	// query from cloud
	cRes := s.batchQueryCloudNonMiss(context.TODO(), mids, cc.BatchSize, cc.BatchMissRetryCount)

	cM := make(map[int64]*model.AsoAccount)
	for _, item := range cRes {
		cM[item.Mid] = item
	}

	// compare cloud with local
	pendingMids := make([]int64, 0)
	for _, item := range lRes {
		local := item
		cloud := cM[item.Mid]
		status := doCompare(cloud, local, true)
		switch status {
		case _statusOK:
			// do nothing
		case _statusNo:
			cc.diffCount++
			s.doLog(cloud, local, false)
			if cc.Fix {
				s.fixCloudRecord(context.TODO(), model.Default(local), cloud)
			}
		case _statusPending:
			pendingMids = append(pendingMids, item.Mid)
		}
	}

	if len(pendingMids) == 0 {
		return
	}

	// reload pending mids from cloud
	var pendingRes []*model.AsoAccount
	if pendingRes, err = s.d.AsoAccountsCloud(context.TODO(), pendingMids); err != nil {
		return
	}

	lM := make(map[int64]*model.OriginAsoAccount)
	for _, item := range lRes {
		lM[item.Mid] = item
	}

	// compare
	for _, item := range pendingRes {
		cloud := item
		local := lM[item.Mid]
		status := doCompare(item, local, false)
		switch status {
		case _statusOK:
		case _statusNo:
			cc.diffCount++
			s.doLog(cloud, local, true)
			if cc.Fix {
				s.fixCloudRecord(context.TODO(), model.Default(local), cloud)
			}
		}
	}
	return
}
