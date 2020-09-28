// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

// +build !windows

package nvidia

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/DataDog/datadog-agent/pkg/aggregator"
	"github.com/DataDog/datadog-agent/pkg/autodiscovery/integration"
	"github.com/DataDog/datadog-agent/pkg/collector/check"
	core "github.com/DataDog/datadog-agent/pkg/collector/corechecks"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"gopkg.in/yaml.v2"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	checkName            = "jetson"

	kb = 1024
	mb = kb * 1024
	gb = mb * 1024

	// Indices of the regex in the 'regexes' variable below
	regexRAMIdx       = 0
	regexSwapCacheIdx = 1
	regexIRamIdx      = 2

	// Regex used to parse the GPU usage and frequency => e.g. EMC_FREQ 7%@408 GR3D_FREQ 0%@76
	regexGpuUsageIdx = 3

	// Regex used to parse the CPU usage section => e.g. CPU [2%@102,1%@102,0%@102,0%@102]
	regexCPUUsageIdx = 4

	// Regex used to parse the temperature information => e.g. thermal@41C
	regexTemperatureIdx = 5

	// Regex used to parse the voltage information => e.g. POM_5V_IN 900/943
	regexVoltageIdx = 6

	// Regex used to parse cpu and freq => e.g. 2%@102
	regexCPUFreqIdx = 7

	// Indices of the matched fields by the RAM regex
	ramUsed          = 1
	totalRAM         = 2
	ramUnit          = 3
	numFreeBlock     = 4
	largestFreeBlock = 5
	lfbUnit          = 6

	// Indices of the matched fields by the Swap/Cache regex
	swapUsed  = 1
	totalSwap = 2
	swapUnit  = 3
	cached    = 4
	cacheUnit = 5

	// Indices of the matched fields by the Icache regex
	iramUsed    = 1
	iramTotal   = 2
	iramUnit    = 3
	iramLfb     = 4
	iramLfbUnit = 5

	// Indices of the matched fields by the GPU usage regex
	emcPct  = 1
	emcFreq = 2
	gpuPct  = 3
	gpuFreq = 4

	// Indices of the matched fields by the voltage regex
	voltageProbeName = 1
	currentVoltage   = 2
	averageVoltage   = 3

	// Indices of the matched fields by the temperature regex
	tempZone  = 1
	tempValue = 2

	// Indices of the matched fields by the CPU regex
	cpuUsage = 1
	cpuFreq  = 2
)

var regexes = [...]string{
	// Group 1.	-> Used
	// Group 2.	-> Total
	// Group 3.	-> Unit
	// Group 4.	-> Number of LFB
	// Group 5.	-> LFB
	// Group 6.	-> Unit
	`RAM\s*(\d+)/(\d+)([kKmMgG][bB])\s*\(lfb\s*(\d+)x(\d+)([kKmMgG][bB])\)`,

	// Group 1.	-> Used
	// Group 2.	-> Total
	// Group 3.	-> Unit
	// Group 4. -> Cached
	// Group 5. -> Unit
	`SWAP\s*(\d+)\/(\d+)([kKmMgG][bB])\s*\(cached\s*(\d+)([kKmMgG][bB])\)`,

	// Group 1.	-> Used
	// Group 2.	-> Total
	// Group 3.	-> Unit
	// Group 4.	-> LFB
	// Group 5.	-> Unit
	`IRAM\s*(\d+)\/(\d+)([kKmMgG][bB])\s*\(lfb\s*(\d+)([kKmMgG][bB])\)`,

	// Group 1.	-> EMC %
	// Group 2.	-> EMC Freq (opt)
	// Group 3.	-> GPU %
	// Group 4.	-> GPU Freq (opt)
	`EMC_FREQ\s*(\d+)%(?:@(\d+))?\s*GR3D_FREQ\s*(\d+)%(?:@(\d+))?`,

	`CPU\s*\[((?:\d+%@\d+,?)+)\]`,

	// Group 1.	-> Zone name
	// Group 2.	-> Temperature
	`(\w+)@(\d+(?:[.]\d+)?)C`,

	// Group 1.	-> Voltage probe name
	// Group 2.	-> Current voltage
	// Group 2.	-> Average voltage
	`(\w+)\s+(\d+)\/(\d+)(?:\s+|$)`,

	// Group 1. -> CPU usage
	// Group 2. -> CPU freq
	`(\d+)%@(\d+)`,
}

type checkCfg struct {
	TegraStatsPath string `yaml:"tegra_stats_path,omitempty"`
}

// JetsonCheck contains the field for the JetsonCheck
type JetsonCheck struct {
	core.CheckBase

	// The path to the tegrastats binary. Defaults to /usr/bin/tegrastats
	tegraStatsPath string

	// The command line options for tegrastats
	commandOpts []string

	regexes []*regexp.Regexp
}

func getSizeMultiplier(unit string) float64 {
	switch strings.ToLower(unit) {
	case "kb":
		return kb
	case "mb":
		return mb
	case "gb":
		return gb
	}
	return 1
}

type sendMetric func(sender aggregator.Sender, field string) error

func (c *JetsonCheck) sendIRAMMetrics(sender aggregator.Sender, field string) error {
	iramFields := c.regexes[regexIRamIdx].FindAllStringSubmatch(field, -1)
	if len(iramFields) != 1 {
		// IRAM is not present on all devices
		return nil
	}

	iramMultiplier := getSizeMultiplier(iramFields[0][iramUnit])

	usedIRAM, err := strconv.ParseFloat(iramFields[0][iramUsed], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.iram.used", usedIRAM*iramMultiplier, "", nil)

	totalIRAM, err := strconv.ParseFloat(iramFields[0][iramTotal], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.iram.total", totalIRAM*iramMultiplier, "", nil)

	iramLfbMultiplier := getSizeMultiplier(iramFields[0][iramLfbUnit])
	iramLfb, err := strconv.ParseFloat(iramFields[0][iramLfb], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.iram.lfb", iramLfb*iramLfbMultiplier, "", nil)

	return nil
}

func (c *JetsonCheck) sendRAMMetrics(sender aggregator.Sender, field string) error {
	ramFields := c.regexes[regexRAMIdx].FindAllStringSubmatch(field, -1)
	if len(ramFields) != 1 {
		return errors.New("could not parse RAM fields")
	}

	ramMultiplier := getSizeMultiplier(ramFields[0][ramUnit])

	usedRAM, err := strconv.ParseFloat(ramFields[0][ramUsed], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.mem.used", usedRAM*ramMultiplier, "", nil)

	totalRAM, err := strconv.ParseFloat(ramFields[0][totalRAM], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.mem.total", totalRAM*ramMultiplier, "", nil)

	// lfb NxXMB, X is the largest free block. N is the number of free blocks of this size.
	lfbMultiplier := getSizeMultiplier(ramFields[0][lfbUnit])

	largestFreeBlock, err := strconv.ParseFloat(ramFields[0][largestFreeBlock], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.mem.lfb", largestFreeBlock*lfbMultiplier, "", nil)

	numFreeBlocks, err := strconv.ParseFloat(ramFields[0][numFreeBlock], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.mem.n_lfb", numFreeBlocks, "", nil)

	return nil
}

func (c *JetsonCheck) sendSwapMetrics(sender aggregator.Sender, field string) error {
	swapFields := c.regexes[regexSwapCacheIdx].FindAllStringSubmatch(field, -1)
	if len(swapFields) != 1 {
		// SWAP is not present on all devices
		return nil
	}

	swapMultiplier := getSizeMultiplier(swapFields[0][swapUnit])

	swapUsed, err := strconv.ParseFloat(swapFields[0][swapUsed], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.swap.used", swapUsed*swapMultiplier, "", nil)

	totalSwap, err := strconv.ParseFloat(swapFields[0][totalSwap], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.swap.total", totalSwap*swapMultiplier, "", nil)

	cacheMultiplier := getSizeMultiplier(swapFields[0][cacheUnit])
	cached, err := strconv.ParseFloat(swapFields[0][cached], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.swap.cached", cached*cacheMultiplier, "", nil)

	return nil
}

func (c *JetsonCheck) sendGpuUsageMetrics(sender aggregator.Sender, field string) error {
	gpuFields := c.regexes[regexGpuUsageIdx].FindAllStringSubmatch(field, -1)
	if len(gpuFields) != 1 {
		return errors.New("could not parse GPU usage fields")
	}

	emcPct, err := strconv.ParseFloat(gpuFields[0][emcPct], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.emc.usage", emcPct, "", nil)

	if len(gpuFields[0][emcFreq]) > 0 {
		emcFreq, err := strconv.ParseFloat(gpuFields[0][emcFreq], 64)
		if err != nil {
			return err
		}
		sender.Gauge("nvidia.jetson.gpu.emc.freq", emcFreq, "", nil)
	}

	gpuPct, err := strconv.ParseFloat(gpuFields[0][gpuPct], 64)
	if err != nil {
		return err
	}
	sender.Gauge("nvidia.jetson.gpu.usage", gpuPct, "", nil)

	if len(gpuFields[0][gpuFreq]) > 0 {
		gpuFreq, err := strconv.ParseFloat(gpuFields[0][gpuFreq], 64)
		if err != nil {
			return err
		}
		sender.Gauge("nvidia.jetson.gpu.freq", gpuFreq, "", nil)
	}

	return nil
}

func (c *JetsonCheck) sendCPUUsageMetrics(sender aggregator.Sender, field string) error {
	cpuFields := c.regexes[regexCPUUsageIdx].FindAllStringSubmatch(field, -1)
	if len(cpuFields) <= 0 {
		return errors.New("could not parse CPU usage fields")
	}
	cpus := strings.Split(cpuFields[0][1], ",")

	for i := 0; i < len(cpus); i++ {
		cpuAndFreqFields := c.regexes[regexCPUFreqIdx].FindAllStringSubmatch(cpus[i], -1)
		cpuUsage, err := strconv.ParseFloat(cpuAndFreqFields[0][cpuUsage], 64)
		cpuTags := []string{fmt.Sprintf("cpu:%d",i)}
		if err != nil {
			return err
		}
		sender.Gauge("nvidia.jetson.cpu.usage", cpuUsage, "", cpuTags)

		cpuFreq, err := strconv.ParseFloat(cpuAndFreqFields[0][cpuFreq], 64)
		if err != nil {
			return err
		}
		sender.Gauge("nvidia.jetson.cpu.freq", cpuFreq, "", cpuTags)
	}

	return nil
}

func (c *JetsonCheck) sendTemperatureMetrics(sender aggregator.Sender, field string) error {
	temperatureFields := c.regexes[regexTemperatureIdx].FindAllStringSubmatch(field, -1)
	if len(temperatureFields) <= 0 {
		return errors.New("could not parse temperature fields")
	}

	for i := 0; i < len(temperatureFields); i++ {
		tempValue, err := strconv.ParseFloat(temperatureFields[i][tempValue], 64)
		if err != nil {
			return err
		}
		temperatureZoneTags := []string{fmt.Sprintf("zone:%s",temperatureFields[i][tempZone])}
		sender.Gauge("nvidia.jetson.gpu.temp", tempValue, "", temperatureZoneTags)
	}

	return nil
}

func (c *JetsonCheck) sendVoltageMetrics(sender aggregator.Sender, field string) error {
	voltageFields := c.regexes[regexVoltageIdx].FindAllStringSubmatch(field, -1)
	if len(voltageFields) <= 0 {
		return errors.New("could not parse voltage fields")
	}

	for i := 0; i < len(voltageFields); i++ {
		voltageProbeTags := []string{fmt.Sprintf("probe:%s", voltageFields[i][voltageProbeName])}
		instantVoltage, err := strconv.ParseFloat(voltageFields[i][currentVoltage], 64)
		if err != nil {
			return err
		}
		sender.Gauge("nvidia.jetson.gpu.vdd.instant", instantVoltage, "", voltageProbeTags)

		averageVoltage, err := strconv.ParseFloat(voltageFields[i][averageVoltage], 64)
		if err != nil {
			return err
		}
		sender.Gauge("nvidia.jetson.gpu.vdd.average", averageVoltage, "", voltageProbeTags)
	}

	return nil
}

func (c *JetsonCheck) processTegraStatsOutput(tegraStatsOuptut string) error {
	sender, err := aggregator.GetSender(c.ID())
	if err != nil {
		return err
	}

	var metricSenders = []sendMetric{
		c.sendRAMMetrics,
		c.sendIRAMMetrics,
		c.sendSwapMetrics,
		c.sendGpuUsageMetrics,
		c.sendCPUUsageMetrics,
		c.sendTemperatureMetrics,
		c.sendVoltageMetrics,
	}
	for _, metricSender := range metricSenders {
		err = metricSender(sender, tegraStatsOuptut)
		if err != nil {
			return err
		}
	}
	sender.Commit()
	return nil
}

// Run executes the check
func (c *JetsonCheck) Run() error {
	cmd := exec.Command(c.tegraStatsPath, c.commandOpts...)

	// Parse the standard output for the stats
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	go func() {
		in := bufio.NewScanner(stdout)
		if in.Scan() {
			// We only need to read one line
			line := in.Text()
			log.Debugf("tegrastats: %s", line)
			if err = c.processTegraStatsOutput(line); err != nil {
				_ = log.Error(err)
			}
		} else {
			_ = log.Warnf("tegrastats did not produce any output")
		}
		err = cmd.Process.Signal(os.Kill)
		if err != nil {
			_ = log.Errorf("unable to stop %s check: %s", checkName, err)
		}
	}()

	// forward the standard error to the Agent logger
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	go func() {
		in := bufio.NewScanner(stderr)
		for in.Scan() {
			_ = log.Error(in.Text())
		}
	}()

	if err := cmd.Start(); err != nil {
		return err
	}

	return nil
}

// Configure the GPU check
func (c *JetsonCheck) Configure(data integration.Data, initConfig integration.Data, source string) error {
	err := c.CommonConfigure(data, source)
	if err != nil {
		return err
	}

	var conf checkCfg
	if err := yaml.Unmarshal(data, &conf); err != nil {
		return err
	}
	if conf.TegraStatsPath != "" {
		c.tegraStatsPath = conf.TegraStatsPath
	} else {
		c.tegraStatsPath = "/usr/bin/tegrastats"
	}

	// We run tegrastats once and then kill the process. However, we set the interval to 500ms
	// because it will take tegrastats <interval> to produce its first output.
	c.commandOpts = []string{
		"--interval",
		"500", // ms
	}

	c.regexes = make([]*regexp.Regexp, len(regexes))
	for idx, regex := range regexes {
		c.regexes[idx] = regexp.MustCompile(regex)
	}

	return nil
}

func jetsonCheckFactory() check.Check {
	return &JetsonCheck{
		CheckBase: core.NewCheckBase(checkName),
	}
}

func init() {
	core.RegisterCheck(checkName, jetsonCheckFactory)
}
