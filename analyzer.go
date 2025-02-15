package mcla

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/kmcsr/go-ringbuf"
)

type SolutionPossibility struct {
	ErrorDesc *ErrorDesc `json:"errorDesc"`
	Match     float32    `json:"match"`
}

type ErrorResult struct {
	Error   *JavaError            `json:"error"`
	Matched []SolutionPossibility `json:"matched"`
	File    string                `json:"file,omitempty"`
}

var (
	ErrCrashReportIncomplete = errors.New("Crashreport is incomplete")
)

type Analyzer struct {
	DB ErrorDB

	errMux        sync.RWMutex
	lastUpdateErr time.Time
	cachedErrors  []*ErrorDesc

	recentMixinLogs *ringbuf.RingBuffer[string]
}

func NewAnalyzer(db ErrorDB) (a *Analyzer) {
	return &Analyzer{
		DB:              db,
		recentMixinLogs: ringbuf.NewRingBuffer[string](64),
	}
}

func (a *Analyzer) UpdateErrors() (err error) {
	a.errMux.Lock()
	defer a.errMux.Unlock()
	return a.updateErrorsLocked()
}

func (a *Analyzer) updateErrorsLocked() (err error) {
	errors := make([]*ErrorDesc, 0, 64)
	if err = a.DB.ForEachErrors(func(e *ErrorDesc) error {
		errors = append(errors, e)
		return nil
	}); err != nil {
		return
	}
	a.lastUpdateErr = time.Now()
	a.cachedErrors = errors
	return
}

func (a *Analyzer) getErrors() []*ErrorDesc {
	a.errMux.RLock()
	needUpdate := a.lastUpdateErr.IsZero() || time.Now().After(a.lastUpdateErr.Add(time.Hour))
	a.errMux.RUnlock()
	if needUpdate {
		a.errMux.Lock()
		if a.lastUpdateErr.IsZero() || time.Now().After(a.lastUpdateErr.Add(time.Hour)) {
			a.updateErrorsLocked()
		}
		a.errMux.Unlock()
	}
	return a.cachedErrors
}

func (a *Analyzer) DoError(jerr *JavaError) (matched []SolutionPossibility, err error) {
	e, _ := a.HardCodedChecks(jerr)
	if e != nil {
		return []SolutionPossibility{
			SolutionPossibility{
				ErrorDesc: e,
				Match:     1,
			},
		}, nil
	}
	epkg, ecls := rsplit(jerr.Class, '.')
	for _, e := range a.getErrors() {
		sol := SolutionPossibility{
			ErrorDesc: e,
		}
		epkg2, ecls2 := rsplit(e.Error, '.')
		ignoreErrorTyp := len(ecls2) == 0 || ecls2 == "*"
		if !ignoreErrorTyp && ecls2 == ecls { // error type weight: 10%
			if epkg2 == "*" || epkg == epkg2 {
				sol.Match = 0.1 // 10%
			} else {
				sol.Match = 0.05 // 5%
			}
		}
		if len(e.Message) == 0 { // when ignore error message, error type provide 100% score weight
			sol.Match /= 10.0 / 100
		} else {
			jemsg, _ := split(jerr.Message, '\n')
			matches := lineMatchPercent(jemsg, e.Message) // error message weight: 90%
			if ignoreErrorTyp {
				sol.Match = matches // or when ignore error type, it provide 100% score weight
			} else {
				sol.Match += matches * 0.9
			}
		}
		if sol.Match != 0 { // have any matches
			matched = append(matched, sol)
		}
	}
	if matched == nil {
		matched = make([]SolutionPossibility, 0)
	}
	return
}

func (a *Analyzer) DoLogStream(c context.Context, r io.Reader) (<-chan *ErrorResult, context.Context) {
	result := make(chan *ErrorResult, 3)
	ctx, cancel := context.WithCancelCause(c)
	go func() {
		defer close(result)
		var wg sync.WaitGroup
		recorder := a.newLogRecorder()
		defer recorder.Close()
		resCh, errCh := ScanJavaErrorsIntoChan(io.TeeReader(r, recorder))
	LOOP:
		for {
			select {
			case jerr := <-resCh:
				if jerr == nil {
					break LOOP
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					for jerr != nil {
						res := &ErrorResult{
							Error: jerr,
						}
						var err error
						if res.Matched, err = a.DoError(jerr); err != nil {
							cancel(err)
							return
						}
						select {
						case result <- res:
						case <-ctx.Done():
							return
						}
						jerr = jerr.CausedBy
					}
				}()
			case err := <-errCh:
				cancel(err)
				return
			case <-ctx.Done():
				return
			}
		}
		wg.Wait()
	}()
	return result, ctx
}

type logRecorder struct {
	a      *Analyzer
	closed bool
	buf    []byte
}

func (a *Analyzer) newLogRecorder() io.WriteCloser {
	a.recentMixinLogs.Clear()
	return &logRecorder{
		a: a,
	}
}

func (r *logRecorder) Write(buf []byte) (int, error) {
	r.buf = append(r.buf, buf...)
	i := 0
	for  {
		j := i + bytes.IndexByte(r.buf[i:], '\n')
		if j < i {
			break
		}
		r.record(r.buf[i:j])
		i = j + 1
	}
	if i > 0 {
		n := copy(r.buf, r.buf[i:])
		r.buf = r.buf[:n]
	}
	return len(buf), nil
}

func (r *logRecorder) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.buf = nil
	return nil
}

var (
	mixinLogRe = regexp.MustCompile(`^\[[^\]]*\]\s*\[[^\]]*\]\s*\[mixin/[^\]]*\]:\s*(.+)$`)
)

func (r *logRecorder) record(buf []byte) {
	matches := mixinLogRe.FindSubmatch(buf)
	if matches != nil {
		r.a.recentMixinLogs.Push((string)(matches[1]))
	}
}
