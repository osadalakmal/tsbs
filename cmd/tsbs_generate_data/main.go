// tsbs_generate_data generates time series data from pre-specified use cases.
//
// Supported formats:
// Cassandra CSV format
// InfluxDB bulk load format
// MongoDB BSON format
// TimescaleDB pseudo-CSV format

// Supported use cases:
// devops: scale-var is the number of hosts to simulate, with log messages
//         every log-interval seconds.
// cpu-only: same as `devops` but only generate metrics for CPU
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/timescale/tsbs/cmd/tsbs_generate_data/common"
	"github.com/timescale/tsbs/cmd/tsbs_generate_data/devops"
	"github.com/timescale/tsbs/cmd/tsbs_generate_data/serialize"
)

const (
	// Output data format choices (alphabetical order)
	formatCassandra   = "cassandra"
	formatInflux      = "influx"
	formatMongo       = "mongo"
	formatTimescaleDB = "timescaledb"

	// Use case choices (make sure to update TestGetConfig if adding a new one)
	useCaseCPUOnly   = "cpu-only"
	useCaseCPUSingle = "cpu-single"
	useCaseDevops    = "devops"

	errTotalGroupsZero  = "incorrect interleaved groups configuration: total groups = 0"
	errInvalidGroupsFmt = "incorrect interleaved groups configuration: id %d >= total groups %d"
	errInvalidFormatFmt = "invalid format specifier: %v (valid choices: %v)"

	inputBufSize = 4 << 20
)

// semi-constants
var (
	formatChoices = []string{formatCassandra, formatInflux, formatMongo, formatTimescaleDB}
	// allows for testing
	fatal = log.Fatalf
)

// parseableFlagVars are flag values that need sanitization or re-parsing after
// being set, e.g., to convert from string to time.Time or re-setting the value
// based on a special '0' value
type parseableFlagVars struct {
	timestampStartStr string
	timestampEndStr   string
	seed              int64
	initScaleVar      uint64
}

// Program option vars:
var (
	format      string
	useCase     string
	profileFile string

	initScaleVar uint64
	scaleVar     uint64
	seed         int64
	debug        int

	timestampStart time.Time
	timestampEnd   time.Time

	interleavedGenerationGroupID uint
	interleavedGenerationGroups  uint

	logInterval time.Duration
)

func parseTimeFromString(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		fatal("%v", err)
		return time.Time{}
	}
	return t.UTC()
}

func validateGroups(groupID, totalGroups uint) (bool, error) {
	if totalGroups == 0 {
		return false, fmt.Errorf(errTotalGroupsZero)
	} else if groupID >= totalGroups {
		return false, fmt.Errorf(errInvalidGroupsFmt, groupID, totalGroups)
	}
	return true, nil
}

func validateFormat(format string) bool {
	for _, s := range formatChoices {
		if s == format {
			return true
		}
	}
	return false
}

func postFlagParse(flags parseableFlagVars) {
	if flags.initScaleVar == 0 {
		initScaleVar = scaleVar
	} else {
		initScaleVar = flags.initScaleVar
	}

	// the default seed is the current timestamp:
	if flags.seed == 0 {
		seed = int64(time.Now().Nanosecond())
	} else {
		seed = flags.seed
	}
	fmt.Fprintf(os.Stderr, "using random seed %d\n", seed)

	// Parse timestamps
	timestampStart = parseTimeFromString(flags.timestampStartStr)
	timestampEnd = parseTimeFromString(flags.timestampEndStr)
}

// Parse args:
func init() {
	pfv := parseableFlagVars{}
	flag.StringVar(&format, "format", "", fmt.Sprintf("Format to emit. (choices: %s)", strings.Join(formatChoices, ", ")))

	flag.StringVar(&useCase, "use-case", "", "Use case to model. (choices: devops, cpu-only)")

	flag.Uint64Var(&pfv.initScaleVar, "initial-scale-var", 0, "Initial scaling variable specific to the use case (e.g., devices in 'devops'). 0 means to use -scale-var value")
	flag.Uint64Var(&scaleVar, "scale-var", 1, "Scaling variable specific to the use case (e.g., devices in 'devops').")

	flag.StringVar(&pfv.timestampStartStr, "timestamp-start", "2016-01-01T00:00:00Z", "Beginning timestamp (RFC3339).")
	flag.StringVar(&pfv.timestampEndStr, "timestamp-end", "2016-01-02T06:00:00Z", "Ending timestamp (RFC3339).")

	flag.Int64Var(&pfv.seed, "seed", 0, "PRNG seed (0 uses the current timestamp). (default 0)")

	flag.IntVar(&debug, "debug", 0, "Debug printing (choices: 0, 1, 2). (default 0)")

	flag.UintVar(&interleavedGenerationGroupID, "interleaved-generation-group-id", 0, "Group (0-indexed) to perform round-robin serialization within. Use this to scale up data generation to multiple processes.")
	flag.UintVar(&interleavedGenerationGroups, "interleaved-generation-groups", 1, "The number of round-robin serialization groups. Use this to scale up data generation to multiple processes.")
	flag.StringVar(&profileFile, "profile-file", "", "File to which to write go profiling data")

	flag.DurationVar(&logInterval, "log-interval", 10*time.Second, "Duration between host data points")
	flag.Parse()

	postFlagParse(pfv)
}

func main() {
	if ok, err := validateGroups(interleavedGenerationGroupID, interleavedGenerationGroups); !ok {
		fatal(err.Error())
	}
	if ok := validateFormat(format); !ok {
		fatal("invalid format specifier: %v (valid choices: %v)", format, formatChoices)
	}

	if len(profileFile) > 0 {
		defer startMemoryProfile(profileFile)()
	}

	rand.Seed(seed)
	out := bufio.NewWriterSize(os.Stdout, inputBufSize)
	defer func() {
		err := out.Flush()
		if err != nil {
			log.Fatal(err.Error())
		}
	}()

	cfg := getConfig(useCase)
	sim := cfg.ToSimulator(logInterval)
	serializer := getSerializer(sim, format, out)

	runSimulator(sim, serializer, out, interleavedGenerationGroupID, interleavedGenerationGroups)
}

func runSimulator(sim common.Simulator, serializer serialize.PointSerializer, out io.Writer, groupID, totalGroups uint) {
	currGroup := uint(0)
	point := serialize.NewPoint()
	for !sim.Finished() {
		write := sim.Next(point)
		if !write {
			point.Reset()
			continue
		}

		// in the default case this is always true
		if currGroup == groupID {
			err := serializer.Serialize(point, out)
			if err != nil {
				fatal("%v", err)
				return
			}
		}
		point.Reset()

		currGroup = (currGroup + 1) % totalGroups
	}
}

func getConfig(useCase string) common.SimulatorConfig {
	switch useCase {
	case useCaseDevops:
		return &devops.DevopsSimulatorConfig{
			Start: timestampStart,
			End:   timestampEnd,

			InitHostCount:   initScaleVar,
			HostCount:       scaleVar,
			HostConstructor: devops.NewHost,
		}
	case useCaseCPUOnly:
		return &devops.CPUOnlySimulatorConfig{
			Start: timestampStart,
			End:   timestampEnd,

			InitHostCount:   initScaleVar,
			HostCount:       scaleVar,
			HostConstructor: devops.NewHostCPUOnly,
		}
	case useCaseCPUSingle:
		return &devops.CPUOnlySimulatorConfig{
			Start: timestampStart,
			End:   timestampEnd,

			InitHostCount:   initScaleVar,
			HostCount:       scaleVar,
			HostConstructor: devops.NewHostCPUSingle,
		}
	default:
		fatal("unknown use case: '%s'", useCase)
		return nil
	}
}

func getSerializer(sim common.Simulator, format string, out *bufio.Writer) serialize.PointSerializer {
	switch format {
	case formatCassandra:
		return &serialize.CassandraSerializer{}
	case formatInflux:
		return &serialize.InfluxSerializer{}
	case formatMongo:
		return &serialize.MongoSerializer{}
	case formatTimescaleDB:
		out.WriteString("tags")
		for _, key := range devops.MachineTagKeys {
			out.WriteString(",")
			out.Write(key)
		}
		out.WriteString("\n")
		// sort the keys so the header is deterministic
		keys := make([]string, 0)
		fields := sim.Fields()
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, measurementName := range keys {
			out.WriteString(measurementName)
			for _, field := range fields[measurementName] {
				out.WriteString(",")
				out.Write(field)

			}
			out.WriteString("\n")
		}
		out.WriteString("\n")

		return &serialize.TimescaleDBSerializer{}
	default:
		fatal("unknown format: '%s'", format)
		return nil
	}
}

// startMemoryProfile sets up memory profiling to be written to profileFile. It
// returns a function to cleanup/write that should be deferred by the caller
func startMemoryProfile(profileFile string) func() {
	f, err := os.Create(profileFile)
	if err != nil {
		log.Fatal("could not create memory profile: ", err)
	}

	stop := func() {
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
		f.Close()
	}

	// Catches ctrl+c signals
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c

		fmt.Fprintln(os.Stderr, "\ncaught interrupt, stopping profile")
		stop()

		os.Exit(0)
	}()

	return stop
}
