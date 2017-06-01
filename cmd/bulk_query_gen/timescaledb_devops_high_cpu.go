package main

import "time"

// TimescaleDBDevopsHighCPU produces TimescaleDB-specific queries for the devops high-cpu cases
type TimescaleDBDevopsHighCPU struct {
	TimescaleDBDevops
	hosts int
}

// NewTimescaleDBDevopsHighCPU produces a new function that produces a new TimescaleDBDevopsHighCPU
func NewTimescaleDBDevopsHighCPU(hosts int) func(DatabaseConfig, time.Time, time.Time) QueryGenerator {
	return func(dbConfig DatabaseConfig, start, end time.Time) QueryGenerator {
		underlying := newTimescaleDBDevopsCommon(dbConfig, start, end).(*TimescaleDBDevops)
		return &TimescaleDBDevopsHighCPU{
			TimescaleDBDevops: *underlying,
			hosts:             hosts,
		}
	}
}

// Dispatch fills in the Query
func (d *TimescaleDBDevopsHighCPU) Dispatch(_, scaleVar int) Query {
	q := NewTimescaleDBQuery() // from pool
	d.HighCPUForHosts(q, scaleVar, d.hosts)
	return q
}