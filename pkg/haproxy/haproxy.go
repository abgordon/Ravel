package haproxy

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
)

// An HAProxy VIPConfig contains an IPV6 address and a trio of arrays
// that signify the service addresses, listen ports, and proxy configuration
// options for each target backend that a VIP is configured for. The VIPConfig
// must be generated in a way that ensures the length and order of each of the
// three arrays is aligned.
type VIPConfig struct {
	Addr6 string

	ServiceAddrs []string
	ListenPorts  []uint16
	ProxyMode    []bool
}

// The HAProxySet provides a simple mechanism for managing a group of HAProxy services for
// multiple source and destination IP addresses. Specifically it provides a mechanism to
// create and reconfigure an HAProxy instance, as well as an instance to stop all running
// instances.
type HAProxySet interface {
	// Configure will create or update an HAProxy Instance.
	Configure(VIPConfig) error

	// StopAll will stop all HAProxy instances.
	// StopAll is blocking until all instances have been destroyed.
	StopAll()

	// StopOne will stop a single HAProxy instance.
	StopOne(listenAddr string)

	GetRemovals(v6Addrs []string) (removals []string)
}

type HAProxySetManager struct {
	sync.Mutex

	sources     map[string]HAProxy
	cancelFuncs map[string]context.CancelFunc
	errChan     chan HAProxyError

	binary    string
	configDir string

	cxl       context.CancelFunc
	ctx       context.Context
	parentCtx context.Context

	services map[string]string

	logger logrus.FieldLogger
}

func NewHAProxySet(ctx context.Context, binary, configDir string, logger logrus.FieldLogger) *HAProxySetManager {

	c2, cxl := context.WithCancel(ctx)

	return &HAProxySetManager{
		sources:     map[string]HAProxy{},
		cancelFuncs: map[string]context.CancelFunc{},
		errChan:     make(chan HAProxyError, 100),

		services: map[string]string{},

		binary:    binary,
		configDir: configDir,
		parentCtx: ctx,
		ctx:       c2,
		cxl:       cxl,

		logger: logger.WithFields(logrus.Fields{"parent": "haproxy"}),
	}
}

// GetRemovals documented in HAProxySet interface
func (h *HAProxySetManager) GetRemovals(v6addrs []string) []string {

	// build a set of currently configured addresses
	h.Lock()
	configured := []string{}
	for addr, _ := range h.sources {
		configured = append(configured, addr)
	}
	h.Unlock()

	// iterate over the inbound set.
	// any inbound address that is not in configured should be
	removals := []string{}
	for _, i := range configured {
		match := false
		for _, j := range v6addrs {
			if i == j {
				match = true
				break
			}
		}
		if !match {
			removals = append(removals, i)
		}
	}
	return removals
}

func (h *HAProxySetManager) StopAll() {
	// TODO: block until all child instances are cleaned up
	h.logger.Debugf("StopAll called")
	h.cxl()

	// rebuild the internal state
	h.sources = map[string]HAProxy{}
	h.cancelFuncs = map[string]context.CancelFunc{}

	h.ctx, h.cxl = context.WithCancel(h.parentCtx)
}

func (h *HAProxySetManager) StopOne(listenAddr string) {
	h.Lock()
	defer h.Unlock()
	h.logger.Debugf("StopOne called for %v", listenAddr)

	if cxl, ok := h.cancelFuncs[listenAddr]; !ok {
		return
	} else {
		cxl()
	}
}

func (h *HAProxySetManager) Configure(config VIPConfig) error {
	listenAddr := config.Addr6
	serviceAddrs := config.ServiceAddrs
	ports := config.ListenPorts

	h.logger.Debugf("configuring s=%v d=%v p=%v", listenAddr, serviceAddrs, ports)
	h.Lock()
	defer h.Unlock()

	// create the instance if it doesn't exist
	if _, found := h.sources[listenAddr]; !found {
		c2, cxl := context.WithCancel(h.ctx)
		instance, err := NewHAProxy(c2, h.binary, h.configDir, listenAddr, serviceAddrs, ports, h.errChan, h.logger)
		if err != nil {
			h.logger.Errorf("error creating new haproxy. canceling context. %v", err)
			cxl()
			return err
		}
		h.sources[listenAddr] = instance
		h.cancelFuncs[listenAddr] = cxl
	}

	// then configure it
	return h.sources[listenAddr].Reload(ports)
}

func (h *HAProxySetManager) run() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case instanceError := <-h.errChan:
			h.logger.Errorf("got error from instance. %v", instanceError.Error)

			// delete the instance that's in an error state, then rebuild a new one and attach it to the sources set
			h.Lock()
			delete(h.sources, instanceError.Source)
			delete(h.cancelFuncs, instanceError.Source)
			c2, cxl := context.WithCancel(h.ctx)
			if instance, err := NewHAProxy(c2, h.binary, h.configDir, instanceError.Source, instanceError.Dest, instanceError.Ports, h.errChan, h.logger); err != nil {
				h.logger.Errorf("error recreating haproxy. canceling context. %v", err)
				cxl()
				h.errChan <- instanceError
			} else {
				h.sources[instanceError.Source] = instance
				h.cancelFuncs[instanceError.Source] = cxl
			}
			h.Unlock()

			// rate limit
			time.Sleep(1000 * time.Millisecond)
		}
	}
}

type HAProxyError struct {
	Error  error
	Source string
	Dest   []string
	Ports  []uint16
}

type HAProxy interface {
	Reload(ports []uint16) error
}

type HAProxyManager struct {
	binary     string
	configDir  string
	listenAddr string

	serviceAddrs []string
	ports        []uint16

	rendered []byte
	template *template.Template

	cmd     *exec.Cmd
	errChan chan HAProxyError

	ctx    context.Context
	logger logrus.FieldLogger
}

type templateContext struct {
	Port   uint16
	Source string
	Dest   string
}

func NewHAProxy(ctx context.Context, binary string, configDir, listenAddr string, serviceAddrs []string, ports []uint16, errChan chan HAProxyError, logger logrus.FieldLogger) (*HAProxyManager, error) {
	t, err := template.New("conf").Parse(haproxyConfig)
	if err != nil {
		return nil, err
	}

	h := &HAProxyManager{
		binary:     binary,
		configDir:  configDir,
		listenAddr: listenAddr,

		serviceAddrs: serviceAddrs,
		ports:        ports,
		errChan:      errChan,

		template: t,
		ctx:      ctx,
		logger:   logger,
	}

	// bootstrap the configuration. this is redundant with the operations in Reload()
	if b, err := h.render(ports); err != nil {
		return nil, fmt.Errorf("error rendering configuration. s=%s d=%v p=%v. %v", h.listenAddr, h.serviceAddrs, ports, err)
	} else if err := h.write(b); err != nil {
		return nil, fmt.Errorf("error writing configuration. s=%s d=%v p=%v. %v", h.listenAddr, h.serviceAddrs, ports, err)
	}

	// spin up the process
	go h.run()

	return h, nil
}

func (h *HAProxyManager) run() {
	args := []string{"-f", h.filename()}
	h.logger.Debugf("starting haproxy with binary %v and args %v", h.binary, args)
	cmd := exec.CommandContext(h.ctx, h.binary, args...)
	h.cmd = cmd

	cmdErr := make(chan error, 1)
	go func() {
		h.logger.Debugf("waiting for exit code")
		cmdErr <- cmd.Run()
		h.logger.Debugf("command exited")
	}()

	for {
		select {
		case <-h.ctx.Done():
			/*
				// Keeping this around as an example of how to gracefully shutdown when the parent context is closed.
				// In this case, HAProxy would progress through SIGUSR1, SIGTERM, finally SIGKILL. What's missing from this
				// is a way to communicate back to the caller that haproxy has been killed.
				// At any rate, get rid of CommandContext and instead deal with the complexity here. Implement HAProxy.Done()
				// or somesuch to deal with the communication factor.

				// if the context completes, the process needs to be stopped gracefully
				if err := h.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
				        h.sendError(fmt.Errorf("haproxy could not receive sigusr1. s=%s d=%s p=%v. %v", h.listenAddr, h.serviceAddrs, h.ports, err))
				        return
				} else {
				        select {
				        case <-time.After(5000 * time.Millisecond):
				        case <-cmdErr:
				                return
				        }
				}

				// okay, so graceful shutdown didn't work. send SIGTERM
				if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				        h.sendError(fmt.Errorf("haproxy could not receive sigterm. s=%s d=%s p=%v. %v", h.listenAddr, h.serviceAddrs, h.ports, err))
				        return
				} else {
				        select {
				        case <-time.After(2000 * time.Millisecond):
				        case <-cmdErr:
				                return
				        }
				}

				// kill the process
				if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
				        h.sendError(fmt.Errorf("haproxy could not receive sigkill. s=%s d=%s p=%v. %v", h.listenAddr, h.serviceAddrs, h.ports, err))
				        return
				}
				return
			*/

		case err := <-cmdErr:
			if err == nil {
				h.logger.Infof("exited without error")
				return
			}
			e2 := fmt.Errorf("haproxy exited with error. s=%s d=%s p=%v. %v", h.listenAddr, h.serviceAddrs, h.ports, err)
			h.logger.Errorf("wat. %v", e2)
			// the the command errors out, we need to report the error
			h.sendError(e2)
			return
		}
	}
}

// Reload rewrites the configuration and sends a signal to HAProxy to initiate the reload
func (h *HAProxyManager) Reload(ports []uint16) error {
	// compare ports and do nothing if they are the same
	if reflect.DeepEqual(ports, h.ports) {
		return nil
	}

	// render template
	b, err := h.render(ports)
	if err != nil {
		return fmt.Errorf("error rendering configuration. s=%s d=%v p=%v. %v", h.listenAddr, h.serviceAddrs, ports, err)
	}

	// write template
	if err := h.write(b); err != nil {
		return fmt.Errorf("error writing configuration. s=%s d=%v p=%v. %v", h.listenAddr, h.serviceAddrs, ports, err)
	}

	// reload haproxy
	if err := h.reload(); err != nil {
		// if things go wrong, unroll the write
		h.unroll()
		return fmt.Errorf("unable to reload haproxy. s=%s d=%v p=%v. %v", h.listenAddr, h.serviceAddrs, ports, err)
	}

	h.rendered = b
	h.ports = ports

	return nil
}

// render accepts a list of ports and renders a valid HAProxy configuration to forward traffic from
// h.listenAddr to h.serviceAddrs on each port.
func (h *HAProxyManager) render(ports []uint16) ([]byte, error) {

	// prepare the context
	d := make([]templateContext, len(ports))
	for i, port := range ports {
		if i == len(h.serviceAddrs) {
			h.logger.Warnf("got port index %d, but only have %d service addrs. ports=%v serviceAddrs=%v", i, len(h.serviceAddrs), ports, h.serviceAddrs)
			continue
		}
		d[i] = templateContext{Port: port, Source: h.listenAddr, Dest: h.serviceAddrs[i]}
	}

	// render the template
	buf := &bytes.Buffer{}
	if err := h.template.Execute(buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// reload sends sighup into the haproxy process
func (h *HAProxyManager) reload() error {
	return h.cmd.Process.Signal(syscall.SIGHUP)
}

// write replaces the existing configuration with the data stored in b, or else creates a new file.
func (h *HAProxyManager) write(b []byte) error {
	f, err := os.OpenFile(h.filename(), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(b)
	return err
}

// filename returns the configuration filename, concatenating the configDir, the ipv6 address, and .conf
func (h *HAProxyManager) filename() string {
	return filepath.Join(h.configDir, h.listenAddr+".conf")
}

// unroll is called by Reload when an error is generated after a new config file is written.
// It overwrites the file on disk with the former configuration.
func (h *HAProxyManager) unroll() {
	if err := h.write(h.rendered); err != nil {
		h.sendError(err)
	}
}

func (h *HAProxyManager) sendError(err error) {
	msg := HAProxyError{
		Error:  fmt.Errorf("unable to unroll haproxy config. config on disk and config in memory may be out of sync. s=%s d=%v. %v", h.listenAddr, h.serviceAddrs, err),
		Source: h.listenAddr,
		Dest:   h.serviceAddrs,
		Ports:  h.ports,
	}
	select {
	case h.errChan <- msg:
	default:
		panic(err)
	}
}
