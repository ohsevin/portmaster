package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/safing/portbase/log"
)

// DNS Classes
const (
	DNSClassMulticast = dns.ClassINET | 1<<15
)

var (
	multicast4Conn *net.UDPConn
	multicast6Conn *net.UDPConn
	unicast4Conn   *net.UDPConn
	unicast6Conn   *net.UDPConn

	questions     = make(map[uint16]*savedQuestion)
	questionsLock sync.Mutex

	mDNSResolver = &Resolver{
		Server:     ServerSourceMDNS,
		ServerType: ServerTypeDNS,
		Source:     ServerSourceMDNS,
		Conn:       &mDNSResolverConn{},
	}
)

type mDNSResolverConn struct{}

func (mrc *mDNSResolverConn) Query(ctx context.Context, q *Query) (*RRCache, error) {
	return queryMulticastDNS(ctx, q)
}

func (mrc *mDNSResolverConn) MarkFailed() {}

func (mrc *mDNSResolverConn) LastFail() time.Time {
	return time.Time{}
}

type savedQuestion struct {
	question dns.Question
	expires  time.Time
	response chan *RRCache
}

func indexOfRR(entry *dns.RR_Header, list *[]dns.RR) int {
	for k, v := range *list {
		if entry.Name == v.Header().Name && entry.Rrtype == v.Header().Rrtype {
			return k
		}
	}
	return -1
}

//nolint:gocyclo,gocognit // TODO: make simpler
func listenToMDNS(ctx context.Context) error {
	var err error
	messages := make(chan *dns.Msg)

	// TODO: init and start every listener in its own service worker
	// this will make the more resilient and actually able to restart

	multicast4Conn, err = net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
	if err != nil {
		// TODO: retry after some time
		log.Warningf("intel(mdns): failed to create udp4 listen multicast socket: %s", err)
	} else {
		module.StartServiceWorker("mdns udp4 multicast listener", 0, func(ctx context.Context) error {
			return listenForDNSPackets(multicast4Conn, messages)
		})
		defer multicast4Conn.Close()
	}

	multicast6Conn, err = net.ListenMulticastUDP("udp6", nil, &net.UDPAddr{IP: net.IP([]byte{0xff, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xfb}), Port: 5353})
	if err != nil {
		// TODO: retry after some time
		log.Warningf("intel(mdns): failed to create udp6 listen multicast socket: %s", err)
	} else {
		module.StartServiceWorker("mdns udp6 multicast listener", 0, func(ctx context.Context) error {
			return listenForDNSPackets(multicast6Conn, messages)
		})
		defer multicast6Conn.Close()
	}

	unicast4Conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		// TODO: retry after some time
		log.Warningf("intel(mdns): failed to create udp4 listen socket: %s", err)
	} else {
		module.StartServiceWorker("mdns udp4 unicast listener", 0, func(ctx context.Context) error {
			return listenForDNSPackets(unicast4Conn, messages)
		})
		defer unicast4Conn.Close()
	}

	unicast6Conn, err = net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6zero, Port: 0})
	if err != nil {
		// TODO: retry after some time
		log.Warningf("intel(mdns): failed to create udp6 listen socket: %s", err)
	} else {
		module.StartServiceWorker("mdns udp6 unicast listener", 0, func(ctx context.Context) error {
			return listenForDNSPackets(unicast6Conn, messages)
		})
		defer unicast6Conn.Close()
	}

	// start message handler
	module.StartServiceWorker("mdns message handler", 0, func(ctx context.Context) error {
		return handleMDNSMessages(ctx, messages)
	})

	// wait for shutdown
	<-module.Ctx.Done()
	return nil
}

//nolint:gocyclo,gocognit // TODO
func handleMDNSMessages(ctx context.Context, messages chan *dns.Msg) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case message := <-messages:
			// log.Tracef("intel: got net mdns message: %s", message)

			var err error
			var question *dns.Question
			var saveFullRequest bool
			scavengedRecords := make(map[string]dns.RR)
			var rrCache *RRCache

			// save every received response
			// if previous save was less than 2 seconds ago, add to response, else replace
			// pick out A and AAAA records and save separately

			// continue if not response
			if !message.Response {
				// log.Tracef("intel: mdns message has no response, ignoring")
				continue
			}

			// continue if rcode is not success
			if message.Rcode != dns.RcodeSuccess {
				// log.Tracef("intel: mdns message has error, ignoring")
				continue
			}

			// continue if answer section is empty
			if len(message.Answer) == 0 {
				// log.Tracef("intel: mdns message has no answers, ignoring")
				continue
			}

			// return saved question
			questionsLock.Lock()
			savedQ := questions[message.MsgHdr.Id]
			questionsLock.Unlock()

			// get question, some servers do not reply with question
			if len(message.Question) > 0 {
				question = &message.Question[0]
				// if questions do not match, disregard saved question
				if savedQ != nil && message.Question[0].String() != savedQ.question.String() {
					savedQ = nil
				}
			} else if savedQ != nil {
				question = &savedQ.question
			}

			if question != nil {
				// continue if class is not INTERNET
				if question.Qclass != dns.ClassINET && question.Qclass != DNSClassMulticast {
					continue
				}
				// mark request to be saved
				saveFullRequest = true
			}

			// get entry from database
			if saveFullRequest {
				rrCache, err = GetRRCache(question.Name, dns.Type(question.Qtype))
				if err != nil || rrCache.updated < time.Now().Add(-2*time.Second).Unix() || rrCache.TTL < time.Now().Unix() {
					rrCache = &RRCache{
						Domain:   question.Name,
						Question: dns.Type(question.Qtype),
					}
				}
			}

			for _, entry := range message.Answer {
				if strings.HasSuffix(entry.Header().Name, ".local.") || domainInScope(entry.Header().Name, localReverseScopes) {
					if saveFullRequest {
						k := indexOfRR(entry.Header(), &rrCache.Answer)
						if k == -1 {
							rrCache.Answer = append(rrCache.Answer, entry)
						} else {
							rrCache.Answer[k] = entry
						}
					}
					switch entry.(type) {
					case *dns.A:
						scavengedRecords[fmt.Sprintf("%sA", entry.Header().Name)] = entry
					case *dns.AAAA:
						scavengedRecords[fmt.Sprintf("%sAAAA", entry.Header().Name)] = entry
					case *dns.PTR:
						if !strings.HasPrefix(entry.Header().Name, "_") {
							scavengedRecords[fmt.Sprintf("%sPTR", entry.Header().Name)] = entry
						}
					}
				}
			}
			for _, entry := range message.Ns {
				if strings.HasSuffix(entry.Header().Name, ".local.") || domainInScope(entry.Header().Name, localReverseScopes) {
					if saveFullRequest {
						k := indexOfRR(entry.Header(), &rrCache.Ns)
						if k == -1 {
							rrCache.Ns = append(rrCache.Ns, entry)
						} else {
							rrCache.Ns[k] = entry
						}
					}
					switch entry.(type) {
					case *dns.A:
						scavengedRecords[fmt.Sprintf("%s_A", entry.Header().Name)] = entry
					case *dns.AAAA:
						scavengedRecords[fmt.Sprintf("%s_AAAA", entry.Header().Name)] = entry
					case *dns.PTR:
						if !strings.HasPrefix(entry.Header().Name, "_") {
							scavengedRecords[fmt.Sprintf("%s_PTR", entry.Header().Name)] = entry
						}
					}
				}
			}
			for _, entry := range message.Extra {
				if strings.HasSuffix(entry.Header().Name, ".local.") || domainInScope(entry.Header().Name, localReverseScopes) {
					if saveFullRequest {
						k := indexOfRR(entry.Header(), &rrCache.Extra)
						if k == -1 {
							rrCache.Extra = append(rrCache.Extra, entry)
						} else {
							rrCache.Extra[k] = entry
						}
					}
					switch entry.(type) {
					case *dns.A:
						scavengedRecords[fmt.Sprintf("%sA", entry.Header().Name)] = entry
					case *dns.AAAA:
						scavengedRecords[fmt.Sprintf("%sAAAA", entry.Header().Name)] = entry
					case *dns.PTR:
						if !strings.HasPrefix(entry.Header().Name, "_") {
							scavengedRecords[fmt.Sprintf("%sPTR", entry.Header().Name)] = entry
						}
					}
				}
			}

			var questionID string
			if saveFullRequest {
				rrCache.Clean(60)
				err := rrCache.Save()
				if err != nil {
					log.Warningf("intel: failed to cache RR %s: %s", rrCache.Domain, err)
				}

				// return finished response
				if savedQ != nil {
					select {
					case savedQ.response <- rrCache:
					default:
					}
				}

				questionID = fmt.Sprintf("%s%s", question.Name, dns.Type(question.Qtype).String())
			}

			for k, v := range scavengedRecords {
				if saveFullRequest && k == questionID {
					continue
				}
				rrCache = &RRCache{
					Domain:   v.Header().Name,
					Question: dns.Type(v.Header().Class),
					Answer:   []dns.RR{v},
				}
				rrCache.Clean(60)
				err := rrCache.Save()
				if err != nil {
					log.Warningf("intel: failed to cache RR %s: %s", rrCache.Domain, err)
				}
				// log.Tracef("intel: mdns scavenged %s", k)
			}

		}

		cleanSavedQuestions()
	}

}

func listenForDNSPackets(conn *net.UDPConn, messages chan *dns.Msg) error {
	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if module.IsStopping() {
				return nil
			}
			log.Debugf("intel: failed to read packet: %s", err)
			return err
		}
		message := new(dns.Msg)
		if err = message.Unpack(buf[:n]); err != nil {
			log.Debugf("intel: failed to unpack message: %s", err)
			continue
		}
		messages <- message
	}
}

func queryMulticastDNS(ctx context.Context, q *Query) (*RRCache, error) {
	// check for active connections
	if unicast4Conn == nil && unicast6Conn == nil {
		return nil, errors.New("unicast mdns connections not initialized")
	}

	// trace log
	log.Tracer(ctx).Trace("intel: resolving with mDNS")

	// create query
	dnsQuery := new(dns.Msg)
	dnsQuery.SetQuestion(q.FQDN, uint16(q.QType))
	// request unicast response
	// q.Question[0].Qclass |= 1 << 15
	dnsQuery.RecursionDesired = false

	// create response channel
	response := make(chan *RRCache)

	// save question
	questionsLock.Lock()
	defer questionsLock.Unlock()
	questions[dnsQuery.MsgHdr.Id] = &savedQuestion{
		question: dnsQuery.Question[0],
		expires:  time.Now().Add(10 * time.Second),
		response: response,
	}

	// pack qeury
	buf, err := dnsQuery.Pack()
	if err != nil {
		return nil, fmt.Errorf("failed to pack query: %s", err)
	}

	// send queries
	if unicast4Conn != nil && uint16(q.QType) != dns.TypeAAAA {
		err = unicast4Conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
		if err != nil {
			return nil, fmt.Errorf("failed to configure query (set timout): %s", err)
		}

		_, err = unicast4Conn.WriteToUDP(buf, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
		if err != nil {
			return nil, fmt.Errorf("failed to send query: %s", err)
		}
	}
	if unicast6Conn != nil && uint16(q.QType) != dns.TypeA {
		err = unicast6Conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
		if err != nil {
			return nil, fmt.Errorf("failed to configure query (set timout): %s", err)
		}

		_, err = unicast6Conn.WriteToUDP(buf, &net.UDPAddr{IP: net.IP([]byte{0xff, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xfb}), Port: 5353})
		if err != nil {
			return nil, fmt.Errorf("failed to send query: %s", err)
		}
	}

	// wait for response or timeout
	select {
	case rrCache := <-response:
		if rrCache != nil {
			return rrCache, nil
		}
	case <-time.After(1 * time.Second):
		// check cache again
		rrCache, err := GetRRCache(q.FQDN, q.QType)
		if err != nil {
			return rrCache, nil
		}
	}

	return nil, ErrNotFound
}

func cleanSavedQuestions() {
	questionsLock.Lock()
	defer questionsLock.Unlock()
	now := time.Now()
	for msgID, savedQuestion := range questions {
		if now.After(savedQuestion.expires) {
			delete(questions, msgID)
		}
	}
}