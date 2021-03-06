package bgp

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.comcast.com/viper-sde/kube2ipvs/pkg/haproxy"
	"github.comcast.com/viper-sde/kube2ipvs/pkg/stats"
	"github.comcast.com/viper-sde/kube2ipvs/pkg/system"
	"github.comcast.com/viper-sde/kube2ipvs/pkg/types"
)

type BGPWorker interface {
	Start() error
	Stop() error
}

type bgpserver struct {
	sync.Mutex

	services map[string]string

	watcher    system.Watcher
	ipLoopback system.IP
	ipPrimary  system.IP
	ipvs       system.IPVS
	bgp        Controller

	doneChan chan struct{}

	lastInboundUpdate time.Time
	lastReconfigure   time.Time

	// haproxy configs
	haproxy haproxy.HAProxySet

	nodes             types.NodesList
	config            *types.ClusterConfig
	lastAppliedConfig *types.ClusterConfig
	newConfig         bool
	nodeChan          chan types.NodesList
	configChan        chan *types.ClusterConfig
	ctxWatch          context.Context
	cxlWatch          context.CancelFunc

	ctx     context.Context
	logger  logrus.FieldLogger
	metrics *stats.WorkerStateMetrics
}

func NewBGPWorker(
	ctx context.Context,
	configKey string,
	watcher system.Watcher,
	ipLoopback system.IP,
	ipPrimary system.IP,
	ipvs system.IPVS,
	bgpController Controller,
	logger logrus.FieldLogger) (BGPWorker, error) {

	logger.Debugf("Enter NewBGPWorker()")
	defer logger.Debugf("Exit NewBGPWorker()")

	haproxy := haproxy.NewHAProxySet(ctx, "/usr/sbin/haproxy", "/etc/ravel", logger)
	logger.Debugf("NewBGPWorker(), haproxy %+v", haproxy)

	r := &bgpserver{
		watcher:    watcher,
		ipLoopback: ipLoopback,
		ipPrimary:  ipPrimary,
		ipvs:       ipvs,
		bgp:        bgpController,

		services: map[string]string{},

		haproxy: haproxy,

		doneChan:   make(chan struct{}),
		configChan: make(chan *types.ClusterConfig, 1),
		nodeChan:   make(chan types.NodesList, 1),

		ctx:     ctx,
		logger:  logger,
		metrics: stats.NewWorkerStateMetrics(stats.KindBGP, configKey),
	}

	logger.Debugf("Exit NewBGPWorker(), return %+v", r)
	return r, nil
}

func (b *bgpserver) Stop() error {
	b.cxlWatch()

	b.logger.Info("blocking until periodic tasks complete")
	select {
	case <-b.doneChan:
	case <-time.After(5000 * time.Millisecond):
	}

	ctxDestroy, cxl := context.WithTimeout(context.Background(), 5000*time.Millisecond)
	defer cxl()

	b.logger.Info("starting cleanup")
	err := b.cleanup(ctxDestroy)
	b.logger.Infof("cleanup complete. error=%v", err)
	return err
}

func (b *bgpserver) cleanup(ctx context.Context) error {
	errs := []string{}

	// Stop all of the HAProxy instances.
	// Not sure whether the best approach is to unpublish the VIPs first, or to
	// close haproxy connections. Depends on whether existing sessions are interrupted
	// when ipLoopback is torn down.
	b.haproxy.StopAll()

	// delete all k2i addresses from loopback
	if err := b.ipLoopback.Teardown(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup - failed to remove ip addresses - %v", err))
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%v", errs)
}

func (b *bgpserver) setup() error {
	b.logger.Debugf("Enter func (b *bgpserver) setup()\n")
	defer b.logger.Debugf("Exit func (b *bgpserver) setup()\n")
	var err error

	// run cleanup
	err = b.cleanup(b.ctx)
	if err != nil {
		return err
	}

	ctxWatch, cxlWatch := context.WithCancel(b.ctx)
	b.cxlWatch = cxlWatch
	b.ctxWatch = ctxWatch

	// register the watcher for both nodes and the configmap
	b.watcher.Nodes(ctxWatch, "bpg-nodes", b.nodeChan)
	b.watcher.ConfigMap(ctxWatch, "bgp-configmap", b.configChan)
	return nil
}

func (b *bgpserver) Start() error {

	b.logger.Debugf("Enter func (b *bgpserver) Start()\n")
	defer b.logger.Debugf("Exit func (b *bgpserver) Start()\n")

	err := b.setup()
	if err != nil {
		return err
	}

	go b.watches()
	go b.periodic()
	return nil
}

// watchServiceUpdates calls the watcher every 100ms to retrieve an updated
// list of service definitions. It then iterates over the map of services and
// builds a new map of namespace/service:port identity to clusterIP:port
func (b *bgpserver) watchServiceUpdates() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
			services := map[string]string{}
			for svcName, svc := range b.watcher.Services() {
				if svc.Spec.ClusterIP == "" {
					continue
				} else if svc.Spec.Ports == nil {
					continue
				}
				for _, port := range svc.Spec.Ports {
					identifier := svcName + ":" + port.Name
					addr := svc.Spec.ClusterIP + ":" + strconv.Itoa(int(port.Port))
					services[identifier] = addr
				}
			}
			b.Lock()
			b.services = services
			b.Unlock()
		}
	}
}

func (b *bgpserver) getClusterAddr(identity string) (string, error) {
	b.Lock()
	defer b.Unlock()
	ip, ok := b.services[identity]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return ip, nil
}

func (b *bgpserver) configure() error {
	logger := b.logger.WithFields(logrus.Fields{"protocol": "ipv4"})
	logger.Debug("Enter func (b *bgpserver) configure()")
	defer logger.Debug("Exit func (b *bgpserver) configure()")

	// add/remove vip addresses on loopback
	err := b.setAddresses()
	if err != nil {
		return err
	}

	// Do something BGP-ish with VIPs from configmap
	// This only adds, and never removes, VIPs
	logger.Debug("applying bgp settings")
	addrs := []string{}
	for ip, _ := range b.config.Config {
		addrs = append(addrs, string(ip))
	}
	err = b.bgp.Set(b.ctx, addrs)
	if err != nil {
		return err
	}

	// Set IPVS rules based on VIPs, pods associated with each VIP
	// and some other settings bgpserver receives from RDEI.
	err = b.ipvs.SetIPVS(b.nodes, b.config, b.logger)
	if err != nil {
		return fmt.Errorf("unable to configure ipvs with error %v", err)
	}
	b.logger.Debug("IPVS configured")
	b.lastReconfigure = time.Now()

	return nil
}

func (b *bgpserver) configure6() error {
	logger := b.logger.WithFields(logrus.Fields{"protocol": "ipv6"})

	logger.Debug("starting configuration")
	// add vip addresses to loopback
	err := b.setAddresses6()
	if err != nil {
		return err
	}

	logger.Debug("configuring haproxy")
	err = b.configureHAProxy()
	if err != nil {
		return err
	}

	logger.Debug("setting up bgp")
	addrs := []string{}
	for ip, _ := range b.config.Config6 {
		addrs = append(addrs, string(ip))
	}
	err = b.bgp.Set(b.ctx, addrs)
	if err != nil {
		return err
	}

	logger.Debug("configuration complete")
	return nil
}

func (b *bgpserver) periodic() {
	b.logger.Debug("Enter func (b *bgpserver) periodic()\n")
	defer b.logger.Debug("Exit func (b *bgpserver) periodic()\n")

	// Queue Depth metric ticker
	queueDepthTicker := time.NewTicker(60 * time.Second)
	defer queueDepthTicker.Stop()

	bgpInterval := 2000 * time.Millisecond
	bgpTicker := time.NewTicker(bgpInterval)
	defer bgpTicker.Stop()

	b.logger.Infof("starting BGP periodic ticker, interval %v", bgpInterval)

	// every so many seconds, reapply configuration without checking parity
	reconfigureDuration := 30 * time.Second
	reconfigureTicker := time.NewTicker(reconfigureDuration)
	defer reconfigureTicker.Stop()

	for {
		select {
		case <-queueDepthTicker.C:
			b.metrics.QueueDepth(len(b.configChan))
			b.logger.Debugf("periodic - config=%+v", b.config)

		case <-reconfigureTicker.C:
			b.logger.Debugf("mandatory periodic reconfigure executing after %v", reconfigureDuration)
			start := time.Now()
			if err := b.configure(); err != nil {
				b.metrics.Reconfigure("critical", time.Now().Sub(start))
				b.logger.Infof("unable to apply mandatory ipv4 reconfiguration. %v", err)
			}

		case <-bgpTicker.C:
			b.logger.Debug("BGP ticker expired, checking parity & etc")
			b.performReconfigure()

		case <-b.ctx.Done():
			b.logger.Info("periodic(): parent context closed. exiting run loop")
			b.doneChan <- struct{}{}
			return
		case <-b.ctxWatch.Done():
			b.logger.Info("periodic(): watch context closed. exiting run loop")
			return
		}
	}
}

func (b *bgpserver) noUpdatesReady() bool {
	return b.lastReconfigure.Sub(b.lastInboundUpdate) > 0
}

func (b *bgpserver) setAddresses6() error {
	// pull existing
	configured, err := b.ipLoopback.Get6()
	if err != nil {
		return err
	}

	// get desired set VIP addresses
	desired := []string{}
	for ip, _ := range b.config.Config6 {
		desired = append(desired, string(ip))
	}

	removals, additions := b.ipLoopback.Compare(configured, desired)
	b.logger.Debugf("additions=%v removals=%v", additions, removals)

	for _, addr := range removals {
		b.logger.WithFields(logrus.Fields{"device": b.ipLoopback.Device(), "addr": addr, "action": "deleting"}).Info()
		if err := b.ipLoopback.Del6(addr); err != nil {
			return err
		}
	}
	for _, addr := range additions {
		b.logger.WithFields(logrus.Fields{"device": b.ipLoopback.Device(), "addr": addr, "action": "adding"}).Info()
		if err := b.ipLoopback.Add6(addr); err != nil {
			return err
		}
	}

	return nil
}

// setAddresses adds or removes IP address from the loopback device (lo).
// The IP addresses should be VIPs, from the configmap that a kubernetes
// watcher gives to a bgpserver in func (b *bgpserver) watches()
func (b *bgpserver) setAddresses() error {
	// pull existing
	configured, err := b.ipLoopback.Get()
	if err != nil {
		return err
	}

	// get desired set VIP addresses
	desired := []string{}
	for ip, _ := range b.config.Config {
		desired = append(desired, string(ip))
	}

	removals, additions := b.ipLoopback.Compare(configured, desired)
	b.logger.Debugf("additions=%v removals=%v", additions, removals)
	b.metrics.LoopbackAdditions(len(additions))
	b.metrics.LoopbackRemovals(len(removals))
	b.metrics.LoopbackTotalDesired(len(desired))
	b.metrics.LoopbackConfigHealthy(1)

	for _, addr := range removals {
		b.logger.WithFields(logrus.Fields{"device": b.ipLoopback.Device(), "addr": addr, "action": "deleting"}).Info()
		if err := b.ipLoopback.Del(addr); err != nil {
			b.metrics.LoopbackRemovalErr(1)
			b.metrics.LoopbackConfigHealthy(0)
			return err
		}
	}
	for _, addr := range additions {
		b.logger.WithFields(logrus.Fields{"device": b.ipLoopback.Device(), "addr": addr, "action": "adding"}).Info()
		if err := b.ipLoopback.Add(addr); err != nil {
			b.metrics.LoopbackAdditionErr(1)
			b.metrics.LoopbackConfigHealthy(0)
			return err
		}
	}

	return nil
}

// TODO: this needs to build a pair of service identifiers and port identifiers
// so, an array of ClusterIP:Port mirrored with an array of listen ports
// configureHAProxy determines whether the VIP should be configured at all, and
// generates a pair of slices of cluster-internal addresses and external listen ports.
func (b *bgpserver) configureHAProxy() error {

	// this is the list of ipv6 addresses
	addrs := []string{}

	// this is the complete set of configurations to be sent to haproxy
	configSet := map[string]haproxy.VIPConfig{}

	// iterating over the ClusterConfig. For each IP address in the config, a PortMap
	// contains mapping of listen ports to service identities.
	for ip, portMap := range b.config.Config {
		// First, look up and store the IPV6 address
		addr6 := string(b.config.IPV6[ip])
		addrs = append(addrs, addr6)

		// next, build up the list of clusterIPs and listenPorts
		serviceAddrs := []string{}
		listenPorts := []uint16{}
		for port, cfg := range portMap {

			// first, get the service identity and look up a cluster address
			identity := cfg.Namespace + "/" + cfg.Service + ":" + cfg.PortName
			if addr4, err := b.getClusterAddr(identity); err != nil {
				b.logger.Errorf("unable to configure haproxy v6 for %v. %v", identity, err)
				continue
			} else {
				serviceAddrs = append(serviceAddrs, addr4)
			}

			// first, get the listen port.
			p, _ := strconv.Atoi(port)
			listenPorts = append(listenPorts, uint16(p))
		}
		configSet[addr6] = haproxy.VIPConfig{
			Addr6:        addr6,
			ServiceAddrs: serviceAddrs,
			ListenPorts:  listenPorts,
		}
	}
	removals := b.haproxy.GetRemovals(addrs)

	b.logger.Debugf("got %d haproxy removals", len(removals))
	for _, removal := range removals {
		b.haproxy.StopOne(removal)
	}

	b.logger.Debugf("got %d haproxy addresses", len(addrs))
	for _, addition := range addrs {
		if err := b.haproxy.Configure(configSet[addition]); err != nil {
			return err
		}
	}

	return nil
}

// watches just selects from node updates and config updates channels,
// setting appropriate instance variable in the receiver b.
// func periodic() will act on any changes in nodes list or config
// when one or more of its timers expire.
func (b *bgpserver) watches() {
	b.logger.Debugf("Enter func (b *bgpserver) watches()\n")
	defer b.logger.Debugf("Exit func (b *bgpserver) watches()\n")

	for {
		select {

		case nodes := <-b.nodeChan:
			b.logger.Debug("recv nodeChan")
			if types.NodesEqual(b.nodes, nodes, b.logger) {
				b.logger.Debug("NODES ARE EQUAL")
				b.metrics.NodeUpdate("noop")
				continue
			}
			b.metrics.NodeUpdate("updated")
			b.logger.Debug("NODES ARE NOT EQUAL")
			b.Lock()
			b.nodes = nodes

			b.lastInboundUpdate = time.Now()
			b.Unlock()

		case configs := <-b.configChan:
			b.logger.Debug("recv configChan")
			b.Lock()
			b.config = configs
			b.newConfig = true
			b.lastInboundUpdate = time.Now()
			b.Unlock()
			b.metrics.ConfigUpdate()

		// Administrative
		case <-b.ctx.Done():
			b.logger.Debugf("parent context closed. exiting run loop")
			return
		case <-b.ctxWatch.Done():
			b.logger.Debugf("watch context closed. exiting run loop")
			return
		}

	}
}

func (b *bgpserver) configReady() bool {
	newConfig := false
	b.Lock()
	if b.newConfig {
		newConfig = true
		b.newConfig = false
	}
	b.Unlock()
	return newConfig
}

// performReconfigure decides whether bgpserver has new
// info that possibly results in an IPVS reconfigure,
// checks to see if that new info would result in an IPVS
// reconfigure, then does it if so.
func (b *bgpserver) performReconfigure() {

	if b.noUpdatesReady() {
		// last update happened before the last reconfigure
		return
	}

	start := time.Now()

	// these are the VIP addresses
	addresses, err := b.ipLoopback.Get()
	if err != nil {
		b.metrics.Reconfigure("error", time.Now().Sub(start))
		b.logger.Infof("unable to compare configurations with error %v", err)
		return
	}

	// compare configurations and apply new IPVS rules if they're different
	same, err := b.ipvs.CheckConfigParity(b.nodes, b.config, addresses, b.configReady())
	if err != nil {
		b.metrics.Reconfigure("error", time.Now().Sub(start))
		b.logger.Infof("unable to compare configurations with error %v", err)
		return
	}

	if same {
		b.logger.Debug("parity same")
		b.metrics.Reconfigure("noop", time.Now().Sub(start))
		return
	}

	b.logger.Debug("parity different, reconfiguring")
	if err := b.configure(); err != nil {
		b.metrics.Reconfigure("critical", time.Now().Sub(start))
		b.logger.Infof("unable to apply ipv4 configuration. %v", err)
		return
	}
	b.metrics.Reconfigure("complete", time.Now().Sub(start))
}
