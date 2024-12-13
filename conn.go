package requests

import (
	"bufio"
	"context"
	"errors"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gospider007/tools"
)

type Conn interface {
	CloseWithError(err error) error
	DoRequest(*http.Request, []string) (*http.Response, error)
}

type conn struct {
	r         *bufio.Reader
	w         *bufio.Writer
	pr        *pipCon
	pw        *pipCon
	conn      net.Conn
	closeFunc func()
}

func newConn(ctx context.Context, con net.Conn, closeFunc func()) *conn {
	c := &conn{
		conn:      con,
		closeFunc: closeFunc,
	}
	c.pr, c.pw = pipe(ctx)
	c.r = bufio.NewReader(c)
	c.w = bufio.NewWriter(c)
	go c.run()
	return c
}
func (obj *conn) Close() error {
	return obj.CloseWithError(nil)
}
func (obj *conn) CloseWithError(err error) error {
	if err == nil {
		err = errors.New("connecotr closeWithError close")
	} else {
		err = tools.WrapError(err, "connecotr closeWithError close")
	}
	if obj.closeFunc != nil {
		obj.closeFunc()
	}
	obj.conn.Close()
	return obj.pr.CloseWitError(err)
}
func (obj *conn) DoRequest(req *http.Request, orderHeaders []string) (*http.Response, error) {
	err := httpWrite(req, obj.w, orderHeaders)
	if err != nil {
		return nil, err
	}
	res, err := http.ReadResponse(obj.r, req)
	if err != nil {
		err = tools.WrapError(err, "http1 read error")
		return nil, err
	}
	if res == nil {
		err = errors.New("response is nil")
	}
	return res, err
}
func (obj *conn) run() (err error) {
	_, err = io.Copy(obj.pw, obj.conn)
	return obj.CloseWithError(err)
}
func (obj *conn) Read(b []byte) (i int, err error) {
	return obj.pr.Read(b)
}
func (obj *conn) Write(b []byte) (int, error) {
	return obj.conn.Write(b)
}
func (obj *conn) LocalAddr() net.Addr {
	return obj.conn.LocalAddr()
}
func (obj *conn) RemoteAddr() net.Addr {
	return obj.conn.RemoteAddr()
}
func (obj *conn) SetDeadline(t time.Time) error {
	return obj.conn.SetDeadline(t)
}
func (obj *conn) SetReadDeadline(t time.Time) error {
	return obj.conn.SetReadDeadline(t)
}
func (obj *conn) SetWriteDeadline(t time.Time) error {
	return obj.conn.SetWriteDeadline(t)
}

type connecotr struct {
	parentForceCtx context.Context //parent force close
	forceCtx       context.Context //force close
	forceCnl       context.CancelCauseFunc
	safeCtx        context.Context //safe close
	safeCnl        context.CancelCauseFunc

	bodyCtx context.Context //body close
	bodyCnl context.CancelCauseFunc

	rawConn Conn
	proxys  []*url.URL
	// inPool  bool
}

func (obj *connecotr) withCancel(forceCtx context.Context, safeCtx context.Context) {
	obj.parentForceCtx = forceCtx
	obj.forceCtx, obj.forceCnl = context.WithCancelCause(forceCtx)
	obj.safeCtx, obj.safeCnl = context.WithCancelCause(safeCtx)
}
func (obj *connecotr) Close() error {
	return obj.rawConn.CloseWithError(errors.New("connecotr Close close"))
}

func (obj *connecotr) wrapBody(task *reqTask) {
	body := new(readWriteCloser)
	obj.bodyCtx, obj.bodyCnl = context.WithCancelCause(task.req.Context())
	body.body = task.res.Body
	body.conn = obj
	task.res.Body = body
}
func (obj *connecotr) httpReq(task *reqTask, done chan struct{}) {
	if task.res, task.err = obj.rawConn.DoRequest(task.req, task.option.OrderHeaders); task.res != nil && task.err == nil {
		obj.wrapBody(task)
	} else if task.err != nil {
		task.err = tools.WrapError(task.err, "http1 roundTrip error")
	}
	close(done)
}

func (obj *connecotr) waitBodyClose() error {
	select {
	case <-obj.bodyCtx.Done(): //wait body close
		if err := context.Cause(obj.bodyCtx); errors.Is(err, errGospiderBodyClose) {
			return nil
		} else {
			return err
		}
	case <-obj.forceCtx.Done(): //force conn close
		return tools.WrapError(context.Cause(obj.forceCtx), "connecotr force close")
	}
}

func (obj *connecotr) taskMain(task *reqTask, waitBody bool) (retry bool) {
	defer func() {
		if retry {
			task.err = nil
			obj.rawConn.CloseWithError(errors.New("taskMain retry close"))
		} else {
			task.cnl()
			if task.err != nil {
				obj.rawConn.CloseWithError(task.err)
			} else if waitBody {
				if err := obj.waitBodyClose(); err != nil {
					obj.rawConn.CloseWithError(err)
				}
			}
		}
	}()
	select {
	case <-obj.safeCtx.Done():
		return true
	case <-obj.forceCtx.Done(): //force conn close
		return true
	default:
	}
	done := make(chan struct{})
	go obj.httpReq(task, done)
	select {
	case <-task.ctx.Done():
		task.err = tools.WrapError(context.Cause(task.ctx), "task.ctx error: ")
		return false
	case <-done:
		if task.err != nil {
			return task.suppertRetry()
		}
		if task.res == nil {
			task.err = context.Cause(task.ctx)
			if task.err == nil {
				task.err = errors.New("response is nil")
			}
			return task.suppertRetry()
		}
		if task.option.Logger != nil {
			task.option.Logger(Log{
				Id:   task.option.requestId,
				Time: time.Now(),
				Type: LogType_ResponseHeader,
				Msg:  "response header",
			})
		}
		return false
	case <-obj.forceCtx.Done(): //force conn close
		err := context.Cause(obj.forceCtx)
		task.err = tools.WrapError(err, "taskMain delete ctx error: ")
		select {
		case <-obj.parentForceCtx.Done():
			return false
		default:
			if errors.Is(err, errConnectionForceClosed) {
				return false
			}
			return true
		}
	}
}

type connPool struct {
	forceCtx context.Context
	forceCnl context.CancelCauseFunc

	safeCtx context.Context
	safeCnl context.CancelCauseFunc

	connKey   string
	total     atomic.Int64
	tasks     chan *reqTask
	connPools *connPools
}

type connPools struct {
	connPools sync.Map
}

func newConnPools() *connPools {
	return new(connPools)
}

func (obj *connPools) get(key string) *connPool {
	val, ok := obj.connPools.Load(key)
	if !ok {
		return nil
	}
	return val.(*connPool)
}

func (obj *connPools) set(key string, pool *connPool) {
	obj.connPools.Store(key, pool)
}

func (obj *connPools) del(key string) {
	obj.connPools.Delete(key)
}
func (obj *connPools) Range() iter.Seq2[string, *connPool] {
	return func(yield func(string, *connPool) bool) {
		obj.connPools.Range(func(key, value any) bool {
			return yield(key.(string), value.(*connPool))
		})
	}
}

func (obj *connPool) notice(task *reqTask) {
	select {
	case obj.tasks <- task:
	case task.emptyPool <- struct{}{}:
	}
}

func (obj *connPool) rwMain(conn *connecotr) {
	conn.withCancel(obj.forceCtx, obj.safeCtx)
	defer func() {
		conn.rawConn.CloseWithError(errors.New("connPool rwMain close"))
		obj.total.Add(-1)
		if obj.total.Load() <= 0 {
			obj.safeClose()
		}
	}()
	if err := conn.waitBodyClose(); err != nil {
		return
	}
	for {
		select {
		case <-conn.safeCtx.Done(): //safe close conn
			return
		case <-conn.forceCtx.Done(): //force close conn
			return
		case task := <-obj.tasks: //recv task
			if task == nil {
				return
			}
			if conn.taskMain(task, true) {
				obj.notice(task)
				return
			}
			if task.err != nil {
				return
			}
		}
	}
}
func (obj *connPool) forceClose() {
	obj.safeClose()
	obj.forceCnl(errors.New("connPool forceClose"))
}

func (obj *connPool) safeClose() {
	obj.connPools.del(obj.connKey)
	obj.safeCnl(errors.New("connPool close"))
}
