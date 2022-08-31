package iop

import (
	"context"
	"os"
	"sync"

	"github.com/flarco/g"
	"github.com/spf13/cast"
)

// Dataflow is a collection of concurrent Datastreams
type Dataflow struct {
	Columns    Columns
	Buffer     [][]interface{}
	StreamCh   chan *Datastream
	Streams    []*Datastream
	Context    g.Context
	Limit      uint64
	InBytes    uint64
	OutBytes   uint64
	deferFuncs []func()
	Ready      bool
	Inferred   bool
	FsURL      string
	readyChn   chan struct{}
	streamMap  map[string]*Datastream
	closed     bool
	mux        sync.Mutex
}

// NewDataflow creates a new dataflow
func NewDataflow(limit ...int) (df *Dataflow) {

	Limit := uint64(0) // infinite
	if len(limit) > 0 && limit[0] != 0 {
		Limit = cast.ToUint64(limit[0])
	}
	ctx := g.NewContext(context.Background())

	df = &Dataflow{
		StreamCh:   make(chan *Datastream, ctx.Wg.Limit),
		Streams:    []*Datastream{},
		Context:    ctx,
		Limit:      Limit,
		streamMap:  map[string]*Datastream{},
		deferFuncs: []func(){},
		readyChn:   make(chan struct{}),
	}

	return
}

// Err return the error if any
func (df *Dataflow) Err() (err error) {
	return df.Context.Err()
}

// IsClosed is true is ds is closed
func (df *Dataflow) IsClosed() bool {
	return df.closed
}

// CleanUp refers the defer functions
func (df *Dataflow) CleanUp() {
	g.Trace("executing defer functions")
	df.mux.Lock()
	defer df.mux.Unlock()
	for i, f := range df.deferFuncs {
		f()
		df.deferFuncs[i] = func() {} // in case it gets called again
	}
}

// Defer runs a given function as close of Dataflow
func (df *Dataflow) Defer(f func()) {
	df.mux.Lock()
	defer df.mux.Unlock()
	if !cast.ToBool(os.Getenv("KEEP_TEMP_FILES")) {
		df.deferFuncs = append(df.deferFuncs, f)
	}
}

// Close closes the df
func (df *Dataflow) Close() {
	if !df.closed {
		close(df.StreamCh)
	}
	df.closed = true
}

// SetReady sets the df.ready
func (df *Dataflow) SetReady() {
	if !df.Ready {
		df.Ready = true
		go func() { df.readyChn <- struct{}{} }()
	}
}

// SetEmpty sets all underlying datastreams empty
func (df *Dataflow) SetEmpty() {
	for _, ds := range df.Streams {
		ds.SetEmpty()
	}
}

// IsEmpty returns true is ds.Rows of all channels as empty
func (df *Dataflow) IsEmpty() bool {
	df.mux.Lock()
	defer df.mux.Unlock()
	for _, ds := range df.Streams {
		if ds != nil && ds.Ready {
			if !ds.IsEmpty() {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

// SetColumns sets the columns
func (df *Dataflow) SetColumns(columns []Column) {
	df.Columns = columns
	// for i := range df.Streams {
	// 	df.Streams[i].Columns = columns
	// 	df.Streams[i].Inferred = true
	// }
}

// ResetStats resets the stats
func (df *Dataflow) ResetStats() {
	for i := range df.Columns {
		// set totals to 0
		df.Columns[i].Stats.TotalCnt = 0
		df.Columns[i].Stats.NullCnt = 0
		df.Columns[i].Stats.StringCnt = 0
		df.Columns[i].Stats.JsonCnt = 0
		df.Columns[i].Stats.IntCnt = 0
		df.Columns[i].Stats.DecCnt = 0
		df.Columns[i].Stats.BoolCnt = 0
		df.Columns[i].Stats.DateCnt = 0
		df.Columns[i].Stats.Checksum = 0
	}
}

// ReplaceStream adds to stream map for downstream ds (mapped or shaped)
func (df *Dataflow) ReplaceStream(old, new *Datastream) {
	new.df = old.df
	if df != nil {
		df.Context.Lock()
		df.streamMap[old.id] = new
		df.Context.Unlock()
		if old.id != new.id {
			g.Trace("datastream `%s` replaced by `%s`", old.id, new.id)
		}
	}
}

// GetFinal returns the final downstream ds (mapped or shaped)
func (df *Dataflow) GetFinal(dsID string) (ds *Datastream) {
	for {
		df.Context.Lock()
		mDs, ok := df.streamMap[dsID]
		df.Context.Unlock()
		if !ok || (ds != nil && mDs.id == ds.id) {
			return
		}
		dsID = mDs.id
		ds = mDs
	}
}

// SyncStats sync stream processor stats aggregated to the df.Columns
func (df *Dataflow) SyncStats() {
	df.ResetStats()

	for _, ds := range df.Streams {
		for i, colStat := range ds.Sp.colStats {
			if i+1 > len(df.Columns) {
				g.Debug("index %d is outside len of array (%d) in SyncStats", i, len(df.Columns))
				continue
			}
			df.Columns[i].Stats.TotalCnt = df.Columns[i].Stats.TotalCnt + colStat.TotalCnt
			df.Columns[i].Stats.NullCnt = df.Columns[i].Stats.NullCnt + colStat.NullCnt
			df.Columns[i].Stats.StringCnt = df.Columns[i].Stats.StringCnt + colStat.StringCnt
			df.Columns[i].Stats.JsonCnt = df.Columns[i].Stats.JsonCnt + colStat.JsonCnt
			df.Columns[i].Stats.IntCnt = df.Columns[i].Stats.IntCnt + colStat.IntCnt
			df.Columns[i].Stats.DecCnt = df.Columns[i].Stats.DecCnt + colStat.DecCnt
			df.Columns[i].Stats.BoolCnt = df.Columns[i].Stats.BoolCnt + colStat.BoolCnt
			df.Columns[i].Stats.DateCnt = df.Columns[i].Stats.DateCnt + colStat.DateCnt
			df.Columns[i].Stats.Checksum = df.Columns[i].Stats.Checksum + colStat.Checksum

			if colStat.Min < df.Columns[i].Stats.Min {
				df.Columns[i].Stats.Min = colStat.Min
			}
			if colStat.Max > df.Columns[i].Stats.Max {
				df.Columns[i].Stats.Max = colStat.Max
			}
			if colStat.MaxLen > df.Columns[i].Stats.MaxLen {
				df.Columns[i].Stats.MaxLen = colStat.MaxLen
			}
			if colStat.MaxDecLen > df.Columns[i].Stats.MaxDecLen {
				df.Columns[i].Stats.MaxDecLen = colStat.MaxDecLen
			}
		}
	}

	if !df.Inferred {
		df.Columns = InferFromStats(df.Columns, false, false)
		df.Inferred = true
	}
}

// Count returns the aggregate count
func (df *Dataflow) Count() (cnt uint64) {
	if df != nil && df.Ready {
		for _, ds := range df.Streams {
			if ds.Ready {
				cnt += ds.Count
			}
		}
	}
	return
}

// AddInBytes add ingress bytes
func (df *Dataflow) AddInBytes(bytes uint64) {
	df.InBytes = df.InBytes + bytes
}

// AddOutBytes add egress bytes
func (df *Dataflow) AddOutBytes(bytes uint64) {
	df.OutBytes = df.OutBytes + bytes
}

func (df *Dataflow) Bytes() (inBytes, outBytes uint64) {
	// outBytes = df.OutBytes // use DsTotalBytes
	// inBytes = df.InBytes // use DsTotalBytes

	dsBytes := df.DsTotalBytes()
	if inBytes == 0 {
		inBytes = dsBytes
	}
	if outBytes == 0 {
		outBytes = dsBytes
	}
	return
}

func (df *Dataflow) DsTotalBytes() (bytes uint64) {
	if df != nil && df.Ready {
		for _, ds := range df.Streams {
			if ds.Ready {
				bytes += ds.Bytes
			}
		}
	}
	return
}

// Size is the number of streams
func (df *Dataflow) Size() int {
	return len(df.Streams)
}

func (df *Dataflow) PushStreamChan(dsCh chan *Datastream) {
	defer df.Close()

	pushCnt := 0

	for ds := range dsCh {
		df.mux.Lock()
		df.Streams = append(df.Streams, ds)
		df.mux.Unlock()

		if df.closed {
			break
		}

		if df.Err() != nil {
			df.Close()
			return
		}

		if ds.Err() != nil {
			df.Context.CaptureErr(ds.Err())
			df.Close()
			return
		}

		select {
		case <-df.Context.Ctx.Done():
			df.Close()
			return
		case <-ds.Context.Ctx.Done():
			df.Close()
			return
		case <-ds.readyChn:
			// wait for first ds to start streaming.
			// columns/buffer need to be populated
			ds.df = df
			if df.Ready {
				// compare columns, if differences than error
				reshape, err := CompareColumns(df.Columns, ds.Columns)
				if err != nil {
					ds.Context.CaptureErr(g.Error(err, "files columns don't match"))
					df.Context.CaptureErr(g.Error(err, "files columns don't match"))
					df.Close()
					return
				} else if reshape {
					ds, err = ds.Shape(df.Columns)
					if err != nil {
						ds.Context.CaptureErr(g.Error(err, "could not reshape ds"))
						df.Context.CaptureErr(g.Error(err, "could not reshape ds"))
						df.Close()
						return
					}
				}
			} else {
				df.Columns = ds.Columns
				df.Buffer = ds.Buffer
				df.Inferred = ds.Inferred
				df.SetReady()
			}

			// wait for stream
			df.ReplaceStream(ds, ds)
			df.StreamCh <- ds
			g.Trace("pushed dss %d", pushCnt)
			pushCnt++
			if df.Limit > 0 && df.Count() >= df.Limit {
				g.Debug("reached dataflow limit of %d", df.Limit)
				return
			}
		}
	}

	if len(df.Streams) == 0 {
		df.SetReady()
	} else {
		g.Debug("pushed %d datastreams", pushCnt)
	}

}

// WaitReady waits until dataflow is ready
func (df *Dataflow) WaitReady() error {
	// wait for first ds to start streaming.
	// columns need to be populated
	select {
	case <-df.readyChn:
		return df.Context.Err()
	case <-df.Context.Ctx.Done():
		return df.Context.Err()
	}
}

// Collect reads from one or more streams and return a dataset
func (df *Dataflow) Collect() (data Dataset, err error) {
	var datas []Dataset

	for ds := range df.StreamCh {
		d, err := ds.Collect(int(df.Limit))
		if err != nil {
			return NewDataset(nil), g.Error(err, "Error collecting ds")
		}

		datas = append(datas, d)
	}

	data.Result = nil
	data.Columns = datas[0].Columns
	data.Rows = [][]interface{}{}

	for _, d := range datas {
		data.Rows = append(data.Rows, d.Rows...)
	}

	return
}

// MergeDataflow merges the dataflow streams into one
func MergeDataflow(df *Dataflow) (ds *Datastream) {

	ds = NewDatastream(df.Columns)

	go func() {
		ds.SetReady()
		defer ds.Close()

	loop:
		for ds0 := range df.StreamCh {
			df.ReplaceStream(ds0, ds)
			for row := range ds0.Rows {
				select {
				case <-df.Context.Ctx.Done():
					ds.Context.CaptureErr(df.Err())
					break loop
				case <-ds.Context.Ctx.Done():
					ds.Context.CaptureErr(ds.Err())
					break loop
				default:
					row = ds.Sp.CastRow(row, ds.Columns)
					ds.Push(row)
				}
			}

			ds0.Buffer = nil // clear buffer
		}
	}()

	return ds
}
