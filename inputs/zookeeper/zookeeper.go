package zookeeper

import (
	crypto_tls "crypto/tls"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/inputs"
	"flashcat.cloud/categraf/pkg/tls"
	"flashcat.cloud/categraf/types"
	"github.com/toolkits/pkg/container/list"
)

const (
	inputName                 = "zookeeper"
	commandNotAllowedTmpl     = "warning: %q command isn't allowed at %q, see '4lw.commands.whitelist' ZK config parameter"
	instanceNotServingMessage = "This ZooKeeper instance is not currently serving requests"
	cmdNotExecutedSffx        = "is not executed because it is not in the whitelist."
)

var (
	versionRE          = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+).*$`)
	metricNameReplacer = strings.NewReplacer("-", "_", ".", "_")
)

type Instance struct {
	Addresses   string            `toml:"addresses"`
	Timeout     int               `toml:"timeout"`
	ClusterName string            `toml:"cluster_name"`
	Labels      map[string]string `toml:"labels"`
	tls.ClientConfig
}

func (i *Instance) ZkHosts() []string {
	return strings.Fields(i.Addresses)
}

func (i *Instance) ZkConnect(host string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: time.Duration(i.Timeout) * time.Second}
	tcpaddr, err := net.ResolveTCPAddr("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve zookeeper(cluster: %s) address: %s: %v", i.ClusterName, host, err)
	}

	if !i.UseTLS {
		return dialer.Dial("tcp", tcpaddr.String())
	}
	tlsConfig, err := i.TLSConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to init tls config: %v", err)
	}
	return crypto_tls.DialWithDialer(&dialer, "tcp", tcpaddr.String(), tlsConfig)
}

type Zookeeper struct {
	config.Interval
	Instances []*Instance `toml:"instances"`

	Counter uint64
	wg      sync.WaitGroup
}

func init() {
	inputs.Add(inputName, func() inputs.Input {
		return &Zookeeper{}
	})
}

func (z *Zookeeper) Prefix() string {
	return ""
}

func (z *Zookeeper) Init() error {
	if len(z.Instances) == 0 {
		return types.ErrInstancesEmpty
	}
	return nil
}

func (z *Zookeeper) Drop() {}

func (z *Zookeeper) Gather(slist *list.SafeList) {
	atomic.AddUint64(&z.Counter, 1)
	for i := range z.Instances {
		ins := z.Instances[i]
		zkHosts := ins.ZkHosts()
		if len(zkHosts) == 0 {
			log.Printf("E! no target zookeeper cluster %s addresses specified", ins.ClusterName)
			continue
		}
		for _, zkHost := range zkHosts {
			z.wg.Add(1)
			go z.gatherOnce(slist, ins, zkHost)
		}
	}
	z.wg.Wait()
}

func (z *Zookeeper) gatherOnce(slist *list.SafeList, ins *Instance, zkHost string) {
	defer z.wg.Done()

	tags := map[string]string{"zk_host": zkHost, "zk_cluster": ins.ClusterName}
	for k, v := range ins.Labels {
		tags[k] = v
	}

	begun := time.Now()

	// scrape use seconds
	defer func(begun time.Time) {
		use := time.Since(begun).Seconds()
		slist.PushFront(inputs.NewSample("zk_scrape_use_seconds", use, tags))
	}(begun)

	// zk_up
	conn, err := ins.ZkConnect(zkHost)
	if err != nil {
		slist.PushFront(inputs.NewSample("zk_up", 0, tags))
		log.Println("E! :"+zkHost, "err:", err)
		return
	}

	defer conn.Close()
	z.gatherMntrResult(conn, slist, ins, tags)

	// zk_ruok
	ruokConn, err := ins.ZkConnect(zkHost)
	if err != nil {
		slist.PushFront(inputs.NewSample("zk_ruok", 0, tags))
		log.Println("E! :"+zkHost, "err:", err)
		return
	}
	defer ruokConn.Close()
	z.gatherRuokResult(ruokConn, slist, ins, tags)
}

func (z *Zookeeper) gatherMntrResult(conn net.Conn, slist *list.SafeList, ins *Instance, globalTags map[string]string) {
	res := sendZookeeperCmd(conn, "mntr")

	// get slice of strings from response, like 'zk_avg_latency 0'
	lines := strings.Split(res, "\n")

	// 'mntr' command isn't allowed in zk config, log as warning
	if strings.Contains(lines[0], cmdNotExecutedSffx) {
		slist.PushFront(inputs.NewSample("zk_up", 0, globalTags))
		log.Printf(commandNotAllowedTmpl, "mntr", conn.RemoteAddr().String())
		return
	}

	slist.PushFront(inputs.NewSample("zk_up", 1, globalTags))

	// skip instance if it in a leader only state and doesnt serving client requests
	if lines[0] == instanceNotServingMessage {
		slist.PushFront(inputs.NewSample("zk_server_leader", 1, globalTags))
		return
	}

	// split each line into key-value pair
	for _, l := range lines {
		if l == "" {
			continue
		}

		kv := strings.Fields(l)
		key := kv[0]
		value := kv[1]

		switch key {
		case "zk_server_state":
			if value == "leader" {
				slist.PushFront(inputs.NewSample("zk_server_leader", 1, globalTags))
			} else {
				slist.PushFront(inputs.NewSample("zk_server_leader", 0, globalTags))
			}

		case "zk_version":
			version := versionRE.ReplaceAllString(value, "$1")
			slist.PushFront(inputs.NewSample("zk_version", 1, globalTags, map[string]string{"version": version}))

		case "zk_peer_state":
			slist.PushFront(inputs.NewSample("zk_peer_state", 1, globalTags, map[string]string{"state": value}))

		default:
			var k string

			if !isDigit(value) {
				log.Printf("warning: skipping metric %q which holds not-digit value: %q", key, value)
				continue
			}
			k = metricNameReplacer.Replace(key)
			if strings.Contains(k, "{") {
				labels := parseLabels(k)
				slist.PushFront(inputs.NewSample(k, value, globalTags, labels))
			} else {
				slist.PushFront(inputs.NewSample(k, value, globalTags))
			}
		}
	}
}

func (z *Zookeeper) gatherRuokResult(conn net.Conn, slist *list.SafeList, ins *Instance, globalTags map[string]string) {
	res := sendZookeeperCmd(conn, "ruok")
	if res == "imok" {
		slist.PushFront(inputs.NewSample("zk_ruok", 1, globalTags))
	} else {
		if strings.Contains(res, cmdNotExecutedSffx) {
			log.Printf(commandNotAllowedTmpl, "ruok", conn.RemoteAddr().String())
		}
		slist.PushFront(inputs.NewSample("zk_ruok", 0, globalTags))
	}
}

func sendZookeeperCmd(conn net.Conn, cmd string) string {
	_, err := conn.Write([]byte(cmd))
	if err != nil {
		log.Println("E! failed to exec Zookeeper command:", cmd)
	}

	res, err := ioutil.ReadAll(conn)
	if err != nil {
		log.Printf("E! failed read Zookeeper command: '%s' response from '%s': %s", cmd, conn.RemoteAddr().String(), err)
	}
	return string(res)
}

func isDigit(in string) bool {
	// check input is an int
	if _, err := strconv.Atoi(in); err != nil {
		// not int, try float
		if _, err := strconv.ParseFloat(in, 64); err != nil {
			return false
		}
	}
	return true
}

func parseLabels(in string) map[string]string {
	labels := map[string]string{}

	labelsRE := regexp.MustCompile(`{(.*)}`)
	labelRE := regexp.MustCompile(`(.*)\=(\".*\")`)
	matchLables := labelsRE.FindStringSubmatch(in)
	if len(matchLables) > 1 {
		labelsStr := matchLables[1]
		for _, labelStr := range strings.Split(labelsStr, ",") {
			m := labelRE.FindStringSubmatch(labelStr)
			if len(m) == 3 {
				key := m[1]
				value := m[2]
				labels[key] = value
			}
		}
	}
	return labels
}