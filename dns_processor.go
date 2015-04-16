package main

import (
	"errors"
	"github.com/miekg/dns"
	"github.com/miekg/unbound"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	TypeMX   = dns.Type(dns.TypeMX)
	TypeA    = dns.Type(dns.TypeA)
	TypeAAAA = dns.Type(dns.TypeAAAA)
	TypeTLSA = dns.Type(dns.TypeTLSA)
)

type DnsQuery struct {
	Domain string
	Type   dns.Type
}

type DnsResult struct {
	// The result
	Results  []string
	Secure   bool
	Error    error
	WhyBogus *string
}

type DnsJob struct {
	// waitGroup for the waiting routines
	wait sync.WaitGroup

	Query  *DnsQuery
	Result *DnsResult
}

type DnsJobs struct {
	jobs []*DnsJob
}

type DnsProcessor struct {
	workers *WorkerPool

	// maps pending/running queries to jobs
	jobs map[DnsQuery]*DnsJob

	// mutex for the map
	mutex sync.Mutex

	// context for Unbound
	unboundCtx *unbound.Unbound

	// Go DNS client
	dnsClient   dns.Client
	dnsResolver string
}

func NewDnsProcessor(workersCount uint) *DnsProcessor {
	proc := &DnsProcessor{}

	work := func(item interface{}) {
		job, ok := item.(*DnsJob)
		if !ok {
			log.Fatal("unexpected object:", item)
		}

		result := proc.Lookup(job.Query)
		job.Result = &result

		// clean up the map
		proc.mutex.Lock()
		delete(proc.jobs, *job.Query)
		proc.mutex.Unlock()

		// wake up the waiting routines
		job.wait.Done()
	}

	proc.workers = NewWorkerPool(workersCount, work)
	proc.unboundCtx = unbound.New()
	proc.jobs = make(map[DnsQuery]*DnsJob)

	return proc
}

func (proc *DnsProcessor) Configure(resolver string, timeout uint) {
	proc.dnsClient.ReadTimeout = time.Duration(timeout) * time.Second
	proc.dnsResolver = resolver
}

// Creates a new job
func (proc *DnsProcessor) NewJob(domain string, typ dns.Type) *DnsJob {
	var query = DnsQuery{Domain: domain, Type: typ}
	var job *DnsJob
	var exist bool

	proc.mutex.Lock()

	// Is the same query already running?
	if job, exist = proc.jobs[query]; !exist {
		job = &DnsJob{}
		job.Query = &query
		job.wait.Add(1)
		proc.jobs[query] = job
	}
	proc.mutex.Unlock()

	if !exist {
		proc.workers.Add(job)
	}

	return job
}

// Creates a group of jobs
func (proc *DnsProcessor) NewJobs(domain string, types []dns.Type) *DnsJobs {
	group := &DnsJobs{}
	for _, typ := range types {
		job := proc.NewJob(domain, typ)
		group.append(job)
	}
	return group
}

// Closes the internal channel and waits until all workers are done
func (proc *DnsProcessor) Close() {
	proc.workers.Close()
}

// Waits until the query is finished
func (job *DnsJob) Wait() {
	job.wait.Wait()
}

// Waits until all queries in this group are finished
func (group *DnsJobs) Wait() {
	for _, job := range group.jobs {
		job.wait.Wait()
	}
}

// Appends a new entry to the result
func (result *DnsResult) append(entry string) {
	result.Results = append(result.Results, entry)
}

func (result *DnsResult) appendRR(rr interface{}) {
	switch record := rr.(type) {
	case *dns.MX:
		result.append(strings.ToLower(strings.TrimSuffix(record.Mx, ".")))
	case *dns.A:
		result.append(record.A.String())
	case *dns.AAAA:
		result.append(record.AAAA.String())
	case *dns.TLSA:
		result.append(strconv.Itoa(int(record.Usage)) +
			" " + strconv.Itoa(int(record.Selector)) +
			" " + strconv.Itoa(int(record.MatchingType)) +
			" " + record.Certificate)
	}
}

// The error string or nil
func (result *DnsResult) ErrorMessage() *string {
	if result.Error == nil {
		return nil
	}
	str := result.Error.Error()
	return &str
}

// Appends a new entry to the result
func (group *DnsJobs) append(job *DnsJob) {
	group.jobs = append(group.jobs, job)
}

// Returns the results
func (job *DnsJob) Results() []string {
	job.Wait()
	return job.Result.Results
}

// Returns the flattened results of all jobs
func (group *DnsJobs) Results() []string {
	results := make([]string, 0)

	for _, job := range group.jobs {
		for _, item := range job.Results() {
			results = append(results, item)
		}
	}
	return results
}

// Does the lookup
func (proc *DnsProcessor) Lookup(query *DnsQuery) (result DnsResult) {

	if query.Type == TypeTLSA {
		// Use unbound (slow) for TLSA lookups
		return proc.lookupUnbound(query)
	} else {
		// Use go-DNS (fast) for all other lookups
		return proc.lookupDns(query)
	}
}

// Loookup using Go-DNS
func (proc *DnsProcessor) lookupDns(query *DnsQuery) (result DnsResult) {
	m := &dns.Msg{}
	m.RecursionDesired = true
	m.SetQuestion(query.Domain, uint16(query.Type))

	// Execute the query
	response, _, err := proc.dnsClient.Exchange(m, proc.dnsResolver)

	// error or NXDomain rcode?
	if err != nil || response.Rcode == dns.RcodeNameError {
		result.Error = err
		return
	}

	// Other erroneous rcode?
	if response.Rcode != dns.RcodeSuccess {
		result.Error = errors.New(dns.RcodeToString[response.Rcode])
		return
	}

	// Append results
	for _, rr := range response.Answer {
		result.appendRR(rr)
	}

	return
}

// Loookup using Unbound
// offers more information on DNSSEC
func (proc *DnsProcessor) lookupUnbound(query *DnsQuery) (result DnsResult) {
	// Execute the query
	response, err := proc.unboundCtx.Resolve(query.Domain, uint16(query.Type), uint16(dns.ClassINET))

	result.Secure = response.Secure

	if response.WhyBogus != "" {
		result.WhyBogus = &response.WhyBogus
	}

	// error or NXDomain rcode?
	if err != nil || response.NxDomain {
		result.Error = err
		return
	}

	// Other erroneous rcode?
	if response.Rcode != dns.RcodeSuccess {
		result.Error = errors.New(dns.RcodeToString[response.Rcode])
		return
	}

	// Append results
	for i, _ := range response.Data {
		result.appendRR(response.Rr[i])
	}

	return
}