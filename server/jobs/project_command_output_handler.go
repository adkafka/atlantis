package jobs

import (
	"sync"
	"time"

	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
)

type OutputBuffer struct {
	OperationComplete bool
	Buffer            []string
}

type PullInfo struct {
	PullNum      int
	Repo         string
	RepoFullName string
	ProjectName  string
	Path         string
	Workspace    string
}

type JobIDInfo struct {
	JobID         string
	JobIDUrl      string
	Time          time.Time
	TimeFormatted string
}

type PullInfoWithJobIDs struct {
	Pull       PullInfo
	JobIDInfos []JobIDInfo
}

type JobInfo struct {
	PullInfo
	HeadCommit string
}

type ProjectCmdOutputLine struct {
	JobID             string
	JobInfo           JobInfo
	Line              string
	OperationComplete bool
}

// AsyncProjectCommandOutputHandler is a handler to transport terraform client
// outputs to the front end.
type AsyncProjectCommandOutputHandler struct {
	projectCmdOutput chan *ProjectCmdOutputLine

	projectOutputBuffers     map[string]OutputBuffer
	projectOutputBuffersLock sync.RWMutex

	receiverBuffers     map[string]map[chan string]bool
	receiverBuffersLock sync.RWMutex

	logger logging.SimpleLogging

	// Tracks all the jobs for a pull request which is used for clean up after a pull request is closed.
	pullToJobMapping sync.Map
}

//go:generate pegomock generate --package mocks -o mocks/mock_project_command_output_handler.go ProjectCommandOutputHandler

type ProjectCommandOutputHandler interface {
	// Send will enqueue the msg and wait for Handle() to receive the message.
	Send(ctx command.ProjectContext, msg string, operationComplete bool)

	SendWorkflowHook(ctx models.WorkflowHookCommandContext, msg string, operationComplete bool)

	// Register registers a channel and blocks until it is caught up. Callers should call this asynchronously when attempting
	// to read the channel in the same goroutine
	Register(jobID string, receiver chan string)

	// Deregister removes a channel from successive updates and closes it.
	Deregister(jobID string, receiver chan string)

	IsKeyExists(key string) bool

	// Listens for msg from channel
	Handle()

	// Cleans up resources for a pull
	CleanUp(pullInfo PullInfo)

	// Returns a map from Pull Requests to Jobs
	GetPullToJobMapping() []PullInfoWithJobIDs
}

func NewAsyncProjectCommandOutputHandler(
	projectCmdOutput chan *ProjectCmdOutputLine,
	logger logging.SimpleLogging,
) ProjectCommandOutputHandler {
	return &AsyncProjectCommandOutputHandler{
		projectCmdOutput:     projectCmdOutput,
		logger:               logger,
		receiverBuffers:      map[string]map[chan string]bool{},
		projectOutputBuffers: map[string]OutputBuffer{},
		pullToJobMapping:     sync.Map{},
	}
}

func (p *AsyncProjectCommandOutputHandler) GetPullToJobMapping() []PullInfoWithJobIDs {

	pullToJobMappings := []PullInfoWithJobIDs{}
	i := 0

	p.pullToJobMapping.Range(func(key, value interface{}) bool {
		pullInfo := key.(PullInfo)
		jobIDMap := value.(map[string]time.Time)

		p := PullInfoWithJobIDs{
			Pull:       pullInfo,
			JobIDInfos: make([]JobIDInfo, 0, len(jobIDMap)),
		}

		for jobID, theTime := range jobIDMap {
			jobIDInfo := JobIDInfo{
				JobID: jobID,
				Time:  theTime,
			}
			p.JobIDInfos = append(p.JobIDInfos, jobIDInfo)
		}

		pullToJobMappings = append(pullToJobMappings, p)
		i++
		return true
	})

	return pullToJobMappings
}

func (p *AsyncProjectCommandOutputHandler) IsKeyExists(key string) bool {
	p.projectOutputBuffersLock.RLock()
	defer p.projectOutputBuffersLock.RUnlock()
	_, ok := p.projectOutputBuffers[key]
	return ok
}

func (p *AsyncProjectCommandOutputHandler) Send(ctx command.ProjectContext, msg string, operationComplete bool) {
	p.projectCmdOutput <- &ProjectCmdOutputLine{
		JobID: ctx.JobID,
		JobInfo: JobInfo{
			HeadCommit: ctx.Pull.HeadCommit,
			PullInfo: PullInfo{
				PullNum:      ctx.Pull.Num,
				Repo:         ctx.BaseRepo.Name,
				RepoFullName: ctx.BaseRepo.FullName,
				ProjectName:  ctx.ProjectName,
				Path:         ctx.RepoRelDir,
				Workspace:    ctx.Workspace,
			},
		},
		Line:              msg,
		OperationComplete: operationComplete,
	}
}

func (p *AsyncProjectCommandOutputHandler) SendWorkflowHook(ctx models.WorkflowHookCommandContext, msg string, operationComplete bool) {
	p.projectCmdOutput <- &ProjectCmdOutputLine{
		JobID: ctx.HookID,
		JobInfo: JobInfo{
			HeadCommit: ctx.Pull.HeadCommit,
			PullInfo: PullInfo{
				PullNum: ctx.Pull.Num,
				Repo:    ctx.BaseRepo.Name,
			},
		},
		Line:              msg,
		OperationComplete: operationComplete,
	}
}

func (p *AsyncProjectCommandOutputHandler) Register(jobID string, receiver chan string) {
	p.addChan(receiver, jobID)
}

func (p *AsyncProjectCommandOutputHandler) Handle() {
	for msg := range p.projectCmdOutput {
		if msg.OperationComplete {
			p.completeJob(msg.JobID)
			continue
		}

		// Add job to pullToJob mapping
		if _, ok := p.pullToJobMapping.Load(msg.JobInfo.PullInfo); !ok {
			p.pullToJobMapping.Store(msg.JobInfo.PullInfo, map[string]time.Time{})
		}
		value, _ := p.pullToJobMapping.Load(msg.JobInfo.PullInfo)
		jobMapping := value.(map[string]time.Time)
		jobMapping[msg.JobID] = time.Now()

		// Forward new message to all receiver channels and output buffer
		p.writeLogLine(msg.JobID, msg.Line)
	}
}

func (p *AsyncProjectCommandOutputHandler) completeJob(jobID string) {
	p.projectOutputBuffersLock.Lock()
	p.receiverBuffersLock.Lock()
	defer func() {
		p.projectOutputBuffersLock.Unlock()
		p.receiverBuffersLock.Unlock()
	}()

	// Update operation status to complete
	if outputBuffer, ok := p.projectOutputBuffers[jobID]; ok {
		outputBuffer.OperationComplete = true
		p.projectOutputBuffers[jobID] = outputBuffer
	}

	// Close active receiver channels
	if openChannels, ok := p.receiverBuffers[jobID]; ok {
		for ch := range openChannels {
			close(ch)
		}
	}

}

func (p *AsyncProjectCommandOutputHandler) addChan(ch chan string, jobID string) {
	p.projectOutputBuffersLock.RLock()
	outputBuffer := p.projectOutputBuffers[jobID]
	p.projectOutputBuffersLock.RUnlock()

	for _, line := range outputBuffer.Buffer {
		ch <- line
	}

	// No need register receiver since all the logs have been streamed
	if outputBuffer.OperationComplete {
		close(ch)
		return
	}

	// add the channel to our registry after we backfill the contents of the buffer,
	// to prevent new messages coming in interleaving with this backfill.
	p.receiverBuffersLock.Lock()
	if p.receiverBuffers[jobID] == nil {
		p.receiverBuffers[jobID] = map[chan string]bool{}
	}
	p.receiverBuffers[jobID][ch] = true
	p.receiverBuffersLock.Unlock()
}

// Add log line to buffer and send to all current channels
func (p *AsyncProjectCommandOutputHandler) writeLogLine(jobID string, line string) {
	p.receiverBuffersLock.Lock()
	for ch := range p.receiverBuffers[jobID] {
		select {
		case ch <- line:
		default:
			// Delete buffered channel if it's blocking.
			delete(p.receiverBuffers[jobID], ch)
		}
	}
	p.receiverBuffersLock.Unlock()

	p.projectOutputBuffersLock.Lock()
	if _, ok := p.projectOutputBuffers[jobID]; !ok {
		p.projectOutputBuffers[jobID] = OutputBuffer{
			Buffer: []string{},
		}
	}
	outputBuffer := p.projectOutputBuffers[jobID]
	outputBuffer.Buffer = append(outputBuffer.Buffer, line)
	p.projectOutputBuffers[jobID] = outputBuffer

	p.projectOutputBuffersLock.Unlock()
}

// Remove channel, so client no longer receives Terraform output
func (p *AsyncProjectCommandOutputHandler) Deregister(jobID string, ch chan string) {
	p.logger.Debug("Removing channel for %s", jobID)
	p.receiverBuffersLock.Lock()
	delete(p.receiverBuffers[jobID], ch)
	p.receiverBuffersLock.Unlock()
}

func (p *AsyncProjectCommandOutputHandler) GetReceiverBufferForPull(jobID string) map[chan string]bool {
	return p.receiverBuffers[jobID]
}

func (p *AsyncProjectCommandOutputHandler) GetProjectOutputBuffer(jobID string) OutputBuffer {
	return p.projectOutputBuffers[jobID]
}

func (p *AsyncProjectCommandOutputHandler) GetJobIDMapForPull(pullInfo PullInfo) map[string]time.Time {
	if value, ok := p.pullToJobMapping.Load(pullInfo); ok {
		return value.(map[string]time.Time)
	}
	return nil
}

func (p *AsyncProjectCommandOutputHandler) CleanUp(pullInfo PullInfo) {
	if value, ok := p.pullToJobMapping.Load(pullInfo); ok {
		jobMapping := value.(map[string]time.Time)
		for jobID := range jobMapping {
			p.projectOutputBuffersLock.Lock()
			delete(p.projectOutputBuffers, jobID)
			p.projectOutputBuffersLock.Unlock()

			p.receiverBuffersLock.Lock()
			delete(p.receiverBuffers, jobID)
			p.receiverBuffersLock.Unlock()
		}

		// Remove job mapping
		p.pullToJobMapping.Delete(pullInfo)
	}
}

// NoopProjectOutputHandler is a mock that doesn't do anything
type NoopProjectOutputHandler struct{}

func (p *NoopProjectOutputHandler) Send(_ command.ProjectContext, _ string, _ bool) {
}

func (p *NoopProjectOutputHandler) SendWorkflowHook(_ models.WorkflowHookCommandContext, _ string, _ bool) {
}

func (p *NoopProjectOutputHandler) Register(_ string, _ chan string) {}

func (p *NoopProjectOutputHandler) Deregister(_ string, _ chan string) {}

func (p *NoopProjectOutputHandler) Handle() {
}

func (p *NoopProjectOutputHandler) CleanUp(_ PullInfo) {
}

func (p *NoopProjectOutputHandler) IsKeyExists(_ string) bool {
	return false
}

func (p *NoopProjectOutputHandler) GetPullToJobMapping() []PullInfoWithJobIDs {
	return []PullInfoWithJobIDs{}
}
