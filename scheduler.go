package cronlib

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// copy robfig/cron's crontab parser to cronlib.cron_parser.go
// "github.com/robfig/cron"

var (
	ErrNotFoundJob     = errors.New("not found job")
	ErrAlreadyRegister = errors.New("the job already in pool")
	ErrJobDOFuncNil    = errors.New("callback func is nil")
	ErrCronSpecInvalid = errors.New("crontab spec is invalid")
)

// null logger
var defualtLogger = func(level, s string) {}

type loggerType func(level, s string)

func SetLogger(logger loggerType) {
	defualtLogger = logger
}

func New() *CronSchduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &CronSchduler{
		tasks:  make(map[string]*JobModel),
		ctx:    ctx,
		cancel: cancel,
		wg:     &sync.WaitGroup{},
		once:   &sync.Once{},
	}
}

type CronSchduler struct {
	tasks  map[string]*JobModel
	ctx    context.Context
	cancel context.CancelFunc

	wg   *sync.WaitGroup
	once *sync.Once

	sync.RWMutex
}

// Register - only register srv's job model, don't start auto.
func (c *CronSchduler) Register(srv string, model *JobModel) error {
	return c.reset(srv, model, true, false)
}

// UpdateJobModel - stop old job, update srv's job model
func (c *CronSchduler) UpdateJobModel(srv string, model *JobModel) error {
	return c.reset(srv, model, false, true)
}

// reset - reset srv model
func (c *CronSchduler) reset(srv string, model *JobModel, denyReplace, autoStart bool) error {
	c.Lock()
	defer c.Unlock()

	// validate model
	err := model.validate()
	if err != nil {
		return err
	}

	model.srv = srv
	model.ctx = c.ctx

	oldModel, ok := c.tasks[srv]
	if denyReplace && ok {
		return ErrAlreadyRegister
	}

	if ok {
		oldModel.kill()
	}

	c.tasks[srv] = model
	if autoStart {
		c.wg.Add(1)
		go c.tasks[srv].runLoop(c.wg)
	}

	return nil
}

// UnRegister - stop and delete srv
func (c *CronSchduler) UnRegister(srv string) error {
	c.Lock()
	defer c.Unlock()

	oldModel, ok := c.tasks[srv]
	if !ok {
		return ErrNotFoundJob
	}

	oldModel.kill()
	delete(c.tasks, srv)
	return nil
}

// Stop - stop all cron job
func (c *CronSchduler) Stop() {
	c.Lock()
	defer c.Unlock()

	for srv, job := range c.tasks {
		job.kill()
		delete(c.tasks, srv)
	}
}

// StopService - stop job by serviceName
func (c *CronSchduler) StopService(srv string) {
	c.Lock()
	defer c.Unlock()

	job, ok := c.tasks[srv]
	if !ok {
		return
	}

	job.kill()
	delete(c.tasks, srv)
}

// StopServicePrefix - stop job by srv regex prefix.
// if regex = "risk.scan", stop risk.scan.total, risk.scan.user at the same time
func (c *CronSchduler) StopServicePrefix(regex string) {
	c.Lock()
	defer c.Unlock()

	// regex match
	for srv, job := range c.tasks {
		if !strings.HasPrefix(srv, regex) {
			continue
		}

		job.kill()
		delete(c.tasks, srv)
	}
}

func validateSpec(spec string) bool {
	_, err := Parse(spec)
	if err != nil {
		return false
	}

	return true
}

func getNextDue(spec string) (time.Time, error) {
	sc, err := Parse(spec)
	if err != nil {
		return time.Now(), err
	}

	due := sc.Next(time.Now())
	return due, err
}

func (c *CronSchduler) Start() {
	// only once call
	c.once.Do(func() {

		for _, job := range c.tasks {
			c.wg.Add(1)
			job.runLoop(c.wg)
		}

	})
}

func (c *CronSchduler) Join() {
	c.wg.Wait()
}

func (c *CronSchduler) GetServiceCron(srv string) (*JobModel, error) {
	c.RLock()
	defer c.RUnlock()

	oldModel, ok := c.tasks[srv]
	if !ok {
		return nil, ErrNotFoundJob
	}

	return oldModel, nil
}

// NewJobModel - defualt block sync callfunc
func NewJobModel(spec string, f func(), options ...JobOption) (*JobModel, error) {
	var err error
	job := &JobModel{
		running:    true,
		async:      false,
		do:         f,
		spec:       spec,
		notifyChan: make(chan int, 1), // avoid block
	}

	for _, opt := range options {
		if opt != nil {
			if err := opt(job); err != nil {
				return nil, err
			}
		}
	}

	err = job.validate()
	if err != nil {
		return job, err
	}

	return job, nil
}

type JobOption func(*JobModel) error

func AsyncMode() JobOption {
	return func(o *JobModel) error {
		o.async = true
		return nil
	}
}

type JobModel struct {
	srv        string
	do         func()
	async      bool
	ctx        context.Context
	notifyChan chan int
	spec       string

	running bool // break for { ... } loop
	exited  bool // ensure job worker is exited already

	sync.RWMutex
}

func (j *JobModel) validate() error {
	if j.do == nil {
		return ErrJobDOFuncNil
	}

	if _, err := getNextDue(j.spec); err != nil {
		return err
	}

	return nil
}

func (j *JobModel) runLoop(wg *sync.WaitGroup) {
	go j.run(wg)
}

func (j *JobModel) run(wg *sync.WaitGroup) {
	var (
		// stdout do time cost
		doTimeCostFunc = func() {
			startTS := time.Now()
			j.do()
			defualtLogger("info",
				fmt.Sprintf("service: %s has been finished, time cost: %s, spec: %s",
					j.srv,
					time.Since(startTS).String(),
					j.spec,
				),
			)
		}

		timer *time.Timer
	)

	// parse crontab spec
	due, err := getNextDue(j.spec)
	interval := due.Sub(time.Now())
	if err != nil {
		panic(err.Error())
	}

	// int timer
	timer = time.NewTimer(interval)

	// release join counter
	defer func() {
		wg.Done()
		j.exited = true
	}()

	for j.running {
		select {
		case <-timer.C:
			due, _ := getNextDue(j.spec)
			timer.Reset(
				due.Sub(time.Now()),
			)

			if j.async {
				go doTimeCostFunc() // goroutine for per job
			} else {
				doTimeCostFunc()
			}

		case <-j.notifyChan:
			// parse crontab spec again !
			continue

		case <-j.ctx.Done():
			return
		}
	}
}

func (j *JobModel) kill() {
	j.running = false
	close(j.notifyChan)
}

func (j *JobModel) workerExited() bool {
	return j.exited
}

func (j *JobModel) notifySig() {
	select {
	case j.notifyChan <- 1:
	default:
		// avoid block
		return
	}
}

