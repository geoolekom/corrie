package writer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"git.aqq.me/go/app/appconf"
	"git.aqq.me/go/app/applog"
	"git.aqq.me/go/app/event"
	"git.aqq.me/go/retrier"
	"github.com/iph0/conf"
	jsoniter "github.com/json-iterator/go"
	"github.com/kak-tus/corrie/message"
	"github.com/kak-tus/corrie/reader"
	"github.com/kshvakov/clickhouse"
)

var wrt *Writer

func init() {
	event.Init.AddHandler(
		func() error {
			cnfMap := appconf.GetConfig()["writer"]

			var cnf writerConfig
			err := conf.Decode(cnfMap, &cnf)
			if err != nil {
				return err
			}

			db, err := sql.Open("clickhouse", cnf.ClickhouseURI)
			if err != nil {
				return err
			}

			err = db.Ping()
			if err != nil {
				exception, ok := err.(*clickhouse.Exception)
				if ok {
					return fmt.Errorf("[%d] %s \n%s", exception.Code, exception.Message, exception.StackTrace)
				}

				return err
			}

			wrt = &Writer{
				logger:     applog.GetLogger().Sugar(),
				config:     cnf,
				db:         db,
				decoder:    jsoniter.Config{UseNumber: true}.Froze(),
				m:          &sync.Mutex{},
				reader:     reader.GetReader(),
				toSendCnts: make(map[string]int),
				toSendVals: make(map[string][]*toSend),
				retrier:    retrier.New(retrier.Config{RetryPolicy: []time.Duration{time.Second * 5}}),
			}

			wrt.logger.Info("Started writer")

			return nil
		},
	)

	event.Stop.AddHandler(
		func() error {
			wrt.logger.Info("Stop writer")

			wrt.reader.Stop()

			wrt.m.Lock()
			wrt.db.Close()

			return nil
		},
	)
}

// GetWriter return instance
func GetWriter() *Writer {
	return wrt
}

// Start writer
func (w Writer) Start() {
	w.m.Lock()

	w.reader.Start()

	start := time.Now()

	for {
		msg, more := <-w.reader.C
		if !more {
			w.sendAll()
			break
		}

		var parsed message.Message
		err := w.decoder.Unmarshal(msg.Body, &parsed)
		if err != nil {
			w.logger.Error("Decode failed: ", err)
			w.reader.ToFailedQueue(msg)

			err := msg.Ack(false)
			if err != nil {
				w.logger.Error("Ack failed: ", err)
			}

			continue
		}

		if w.toSendVals[parsed.Query] == nil {
			w.toSendVals[parsed.Query] = make([]*toSend, w.config.Batch)
			w.toSendCnts[parsed.Query] = 0
		}

		w.toSendVals[parsed.Query][w.toSendCnts[parsed.Query]] = &toSend{
			parsed:  parsed,
			nanachi: msg,
			failed:  false,
		}

		w.toSendCnts[parsed.Query]++

		if w.toSendCnts[parsed.Query] >= w.config.Batch {
			w.sendOne(parsed.Query)
		}

		if time.Now().Sub(start).Seconds() >= float64(w.config.Period) {
			w.sendAll()
			start = time.Now()
		}
	}

	w.m.Unlock()
}

// IsAccessible checks Clickhouse status
func (w Writer) IsAccessible() bool {
	for i := 0; i < 10; i++ {
		err := w.db.Ping()
		if err == nil {
			return true
		}

		w.logger.Error("Ping failed: ", err)
		time.Sleep(time.Second)
	}

	return false
}

func (w *Writer) sendAll() {
	for query := range w.toSendVals {
		w.sendOne(query)
	}
}

func (w *Writer) sendOne(query string) {
	if w.toSendCnts[query] > 0 {
		started := time.Now()
		w.send(query, w.toSendVals[query][0:w.toSendCnts[query]])

		diff := time.Now().Sub(started)
		w.logger.Infof("Sended %d values in %fsec for %q", w.toSendCnts[query], diff.Seconds(), query)

		for _, v := range w.toSendVals[query][0:w.toSendCnts[query]] {
			if v.failed {
				w.reader.ToFailedQueue(v.nanachi)
			}

			err := v.nanachi.Ack(false)
			if err != nil {
				w.logger.Error("Ack failed: ", err)
			}
		}

		w.toSendCnts[query] = 0
	}
}

func (w *Writer) send(query string, vals []*toSend) {
	w.retrier.Do(func() *retrier.Error {
		tx, err := w.db.Begin()
		if err != nil {
			w.logger.Error("Start transaction failed: ", err)
			return retrier.NewError(err, false)
		}

		stmt, err := tx.Prepare(query)
		if err != nil {
			tx.Rollback()
			w.logger.Error("Prepare query failed: ", err)

			for _, v := range vals {
				v.failed = true
			}

			return nil
		}

		// There is no need to commit if no one succeeded exec
		succeded := 0

		for _, v := range vals {
			if v.failed {
				continue
			}

			data := w.makeCHArray(v.parsed.Data)
			_, err := stmt.Exec(data...)

			if err != nil {
				w.logger.Error("Exec failed: ", err)
				v.failed = true
				continue
			}

			succeded++
		}

		if succeded == 0 {
			tx.Rollback()
			return nil
		}

		err = tx.Commit()
		if err != nil {
			w.logger.Error("Commit failed: ", err)
			return retrier.NewError(err, false)
		}

		return nil
	})
}

func (w Writer) makeCHArray(vals []interface{}) []interface{} {
	data := make([]interface{}, len(vals))

	for i, v := range vals {
		num, ok := v.(json.Number)

		if !ok {
			data[i] = v
			continue
		}

		convI, err := num.Int64()
		if err == nil {
			data[i] = convI
			continue
		}

		convF, err := num.Float64()
		if err == nil {
			data[i] = convF
			continue
		}

		data[i] = v
	}

	return data
}
