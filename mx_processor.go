package main

import (
	"github.com/miekg/dns"
	"net"
)

var (
	addressTypes = []dns.Type{TypeA, dns.Type(TypeAAAA)}
)

type MxProcessor struct {
	workers *WorkerPool
}

func NewMxProcessor(workersCount uint) *MxProcessor {
	work := func(item interface{}) {
		hostname, _ := item.(string)

		// Do the A/AAAA lookups
		mxAddresses := dnsProcessor.NewJobs(hostname, addressTypes)
		mxAddresses.Wait()

		// Save results
		resultProcessor.Add(mxAddresses)

		// Make addresses unique
		addresses := UniqueStrings(mxAddresses.Results())

		// Do the bannergrabs
		jobs := make([]*ZgrabJob, len(addresses))

		for i, addr := range addresses {
			jobs[i] = zgrabProcessor.NewJob(net.ParseIP(addr))
		}

		for _, job := range jobs {
			job.Wait()
		}

	}

	return &MxProcessor{workers: NewWorkerPool(workersCount, work)}
}

// Creates a new job
func (proc *MxProcessor) NewJob(hostname string) {
	proc.workers.Add(hostname)
}

// Creates a new job
func (proc *MxProcessor) Close() {
	proc.workers.Close()
}
