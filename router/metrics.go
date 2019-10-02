package router

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type MetricSample struct {
	ContainerName string
	PumpId        string
	LogCount      int
}

type DeadPumpAlert struct {
	PumpId  string
	DeadFor time.Duration
}

type InfluxDbPointModel struct {
	MeasurementName string
	Tags            map[string]*string
	Fields          map[string]*string
	Timestamp       int64
}

const newlineByte = byte('\n')
const escapeByte = byte('\\')
const spaceByte = byte(' ')
const commaByte = byte(',')
const equalsByte = byte('=')

const metricsChannelSize = 10000
const deadLogStreamAlertChannelSize = 100
const deadLogStreamThresholdFudgeFactor = float64(6)
const metricHistorySampleCount = 20
const metricsChannelFlushIntervalString = "100ms"
const selfMetricBufferSizeBytes = 4096

var logMetrics = "0"
var metricsFlushIntervalString = "10s"
var metricsProtocol = "udp"

// default docker bridge IP address (Ip address of host machine from containers perspective)  and default UDP port for telegraf, 8092
var metricsHostPort = "172.17.0.1:8092"

var metricsDatabaseName = "aws"
var metricName = "logspout_log_count_by_container"

var metricsChannelFlushInterval time.Duration
var metricsFlushInterval time.Duration

var metricChannel chan MetricSample
var deadLogStreamAlertChannel chan DeadPumpAlert
var metricBuffer map[string]*MetricSample
var metricHistory map[string][]MetricSample
var lastMetricSent time.Time

func init() {
	metricsProtocol = getopt("METRICS_PROTOCOL", metricsProtocol)
	metricsHostPort = getopt("METRICS_HOST_PORT", metricsHostPort)
	metricsDatabaseName = getopt("METRICS_DATABASE_NAME", metricsDatabaseName)
	metricName = getopt("METRIC_NAME", metricName)
	logMetrics = getopt("LOG_METRICS", logMetrics)
	metricsFlushIntervalString = getopt("METRICS_FLUSH_INTERVAL", metricsFlushIntervalString)

	var err error
	metricsChannelFlushInterval, err = time.ParseDuration(metricsChannelFlushIntervalString)
	if err != nil {
		panic(err)
	}
	metricsFlushInterval, err = time.ParseDuration(metricsFlushIntervalString)
	if err != nil {
		panic(err)
	}
	metricChannel = make(chan MetricSample, metricsChannelSize)
	deadLogStreamAlertChannel = make(chan DeadPumpAlert, deadLogStreamAlertChannelSize)
	lastMetricSent = time.Now()

	metricBuffer = make(map[string]*MetricSample)
	metricHistory = make(map[string][]MetricSample)

	metricsChannelFlushInterval, err = time.ParseDuration(metricsChannelFlushIntervalString)
	if err != nil {
		panic(err)
	}

	if metricsProtocol != "udp" && metricsProtocol != "http" && metricsProtocol != "https" {
		panic(fmt.Errorf("Unsupported METRICS_PROTOCOL: %s. Supported protocols: http, https, udp", metricsProtocol))
	}

	go aggregateAndSendMetrics()
}

func checkMetricHistoryForDeadLogStreams() {
	//bytes, _ := json.MarshalIndent(metricHistory, "", "  ")
	//fmt.Printf("checkMetricHistoryForDeadLogStreams %s\n", string(bytes))
	for tagValuesCSV, logStreamHistory := range metricHistory {

		//fmt.Printf("tagValuesCSV: %s, checkMetricHistoryForDeadLogStreams %d >= %d ? \n", tagValuesCSV, len(logStreamHistory), metricHistorySampleCount)
		if len(logStreamHistory) < metricHistorySampleCount {
			continue
		}

		numberOfSamplesInARowThatHadZeroLogs := len(logStreamHistory)
		for i := 0; i < len(logStreamHistory); i++ {

			if logStreamHistory[i].LogCount != 0 {
				numberOfSamplesInARowThatHadZeroLogs = i
				break
			}
		}
		//fmt.Printf("tagValuesCSV: %s, numberOfSamplesInARowThatHadZeroLogs: %d\n", tagValuesCSV, numberOfSamplesInARowThatHadZeroLogs)

		//  So there was at least  one sample where there were zero logs.
		//  is that / are those zero(s) an anomaly? Or is it normal?
		//  Well, how many standard deviations away from the mean is zero?
		//  we compare how many standard deviations away it is with the number of samples in a row that were zero, adjusted with a fudge factor.
		//  So lets say zero is two standard deviations away from the mean and we only saw one sample in a row with zero logs.
		//  then we would say 2 std devs > fudgefactor(1.5) / # of zero-logs-samples-in-a-row(1) .  That would evaluate to true, meaning, the log stream has stopped.
		//  So lets say zero is one standard deviations away from the mean and we only saw one sample in a row with zero logs.
		//  then we would say  1 std devs > fudgefactor(1.5) / # of zero-logs-samples-in-a-row(1) .  That would evaluate to false, meaning this is normal.
		//  but if number of zero-logs-samples-in-a-row were to increase to 2, then it would evaluate to true, indicating that the stream has stopped.

		if numberOfSamplesInARowThatHadZeroLogs > 0 {
			previousSamplesLogCountSum := 0
			previousSamplesCount := 0
			for i := numberOfSamplesInARowThatHadZeroLogs; i < len(logStreamHistory); i++ {
				previousSamplesLogCountSum += logStreamHistory[i].LogCount
				previousSamplesCount++
			}
			previousSamplesAverageLogCount := (float64(previousSamplesLogCountSum) / float64(previousSamplesCount))
			sumOfSquaresOfDeviationFromMean := float64(0)
			for i := numberOfSamplesInARowThatHadZeroLogs; i < len(logStreamHistory); i++ {
				deviation := float64(logStreamHistory[i].LogCount) - previousSamplesAverageLogCount
				sumOfSquaresOfDeviationFromMean += (deviation * deviation)
			}
			standardDeviation := math.Sqrt(sumOfSquaresOfDeviationFromMean / float64(previousSamplesCount))

			distanceFromAverageToZeroInStandardDeviations := previousSamplesAverageLogCount / standardDeviation
			logStreamIsDead := distanceFromAverageToZeroInStandardDeviations > deadLogStreamThresholdFudgeFactor/float64(numberOfSamplesInARowThatHadZeroLogs)
			// numberOfSamplesInARowThatHadZeroLogs: 1, distanceFromAverageToZeroInStandardDeviations: 0.55, logStreamIsDead: fals

			// debugLog(fmt.Sprintf(
			// 	"tagValuesCSV: %s, numberOfSamplesInARowThatHadZeroLogs: %d, distanceFromAverageToZeroInStandardDeviations: %.2f, logStreamIsDead: %t\n",
			// 	tagValuesCSV,
			// 	numberOfSamplesInARowThatHadZeroLogs,
			// 	distanceFromAverageToZeroInStandardDeviations,
			// 	logStreamIsDead,
			// ))
			if logStreamIsDead {
				tagValuesSlice := strings.Split(tagValuesCSV, ",")
				deadLogStreamAlertChannel <- DeadPumpAlert{
					PumpId:  tagValuesSlice[0],
					DeadFor: time.Duration(numberOfSamplesInARowThatHadZeroLogs) * metricsFlushInterval,
				}
			}
		}

	}
}

func aggregateAndSendMetrics() {

	if metricChannel != nil {
		done := false
		for !done {
			select {
			case s := <-metricChannel:
				tagsString := fmt.Sprintf(
					"%s,%s",
					s.PumpId,
					s.ContainerName,
				)
				if metricBuffer[tagsString] == nil {
					s.LogCount = 1
					metricBuffer[tagsString] = &s
				} else {
					b := metricBuffer[tagsString]
					b.LogCount++
				}
			default:
				// break does not work here :\
				done = true
			}
		}
	} else {
		log.Println("Error: metricChannel is nil!")
	}

	if time.Since(lastMetricSent) > metricsFlushInterval {

		buffer := bytes.NewBuffer(make([]byte, 0, selfMetricBufferSizeBytes))
		i := 0

		for tagsString, sample := range metricBuffer {
			_, has := metricHistory[tagsString]
			if !has {
				metricHistory[tagsString] = make([]MetricSample, 0, metricHistorySampleCount)
			}

			Count := fmt.Sprintf("%d", sample.LogCount)
			metricPoint := InfluxDbPointModel{
				MeasurementName: metricName,
				Fields:          map[string]*string{"Count": &Count},
				Tags: map[string]*string{
					"ContainerName": &sample.ContainerName,
					"PumpId":        &sample.PumpId,
				},
				Timestamp: time.Now().UnixNano(),
			}

			writePoint(buffer, &metricPoint, i < len(metricBuffer)-1)
			i++
		}

		newMetricHistory := make(map[string][]MetricSample)

		for tagsString, historyForTheseTags := range metricHistory {
			sample, hasSample := metricBuffer[tagsString]
			if !hasSample {
				tagValues := strings.Split(tagsString, ",")
				sample = &MetricSample{
					PumpId:        tagValues[0],
					ContainerName: tagValues[1],
					LogCount:      0,
				}
			}

			newHistoryLength := intmin(metricHistorySampleCount, len(historyForTheseTags)+1)
			newHistory := make([]MetricSample, newHistoryLength)
			newHistory[0] = *sample
			for i = 1; i < newHistoryLength; i++ {
				newHistory[i] = historyForTheseTags[i-1]
			}
			newMetricHistory[tagsString] = newHistory
		}

		metricHistory = newMetricHistory

		checkMetricHistoryForDeadLogStreams()

		metricBuffer = make(map[string]*MetricSample)

		if logMetrics == "1" {
			log.Println(string(buffer.Bytes()))
		}

		if metricsProtocol == "http" || metricsProtocol == "https" {
			go func(buffer *bytes.Buffer) {
				response, err := http.Post(fmt.Sprintf("%s://%s/write?db=%s", metricsProtocol, metricsHostPort, metricsDatabaseName), "text/plain", buffer)
				if err != nil {
					log.Printf("Error attempting to report self metrics: %+v\n", err)
				} else if response.StatusCode < 200 || response.StatusCode >= 300 {
					responseBodyString := ""
					if response.Body != nil {
						responseBody := bytes.NewBuffer(make([]byte, 0))
						_, err = io.Copy(responseBody, response.Body)
						if err == nil {
							responseBodyString = string(responseBody.Bytes())
						}
					}
					log.Printf("HTTP %d (%s) attempting to report self metrics: %s", response.StatusCode, response.Status, responseBodyString)
				}
			}(buffer)
		} else if metricsProtocol == "udp" {
			go func(buffer *bytes.Buffer) {
				connection, err := net.Dial("udp", metricsHostPort)
				if err != nil {
					log.Printf("Error attempting to net.Dial(\"udp\", metricConfig.HostPort) to report self metrics: %+v\n", err)
					return
				}

				defer connection.Close()

				_, err = connection.Write(buffer.Bytes())
				if err != nil {
					log.Printf("Error attempting to connection.Write to report self metrics: %+v\n", err)
					return
				}
			}(buffer)
		} else {
			log.Printf("Can't send metrics because: Unsupported metric protocol: %s. Supported protocols: http, https, udp", metricsProtocol)
		}

		lastMetricSent = time.Now()
	}

	time.AfterFunc(metricsChannelFlushInterval, aggregateAndSendMetrics)
}

func writePoint(writer *bytes.Buffer, point *InfluxDbPointModel, hasMore bool) {
	writer.Write([]byte(point.MeasurementName))
	tagsCount := len(point.Tags)
	tagIndex := 0
	if tagsCount > 0 {
		writer.WriteByte(commaByte)
	}
	tagKeys := make([][]byte, 0, len(point.Tags))
	for k := range point.Tags {
		tagKeys = append(tagKeys, []byte(k))
	}
	sortedTagKeys := SortByteSlices(tagKeys)
	for _, k := range sortedTagKeys {
		writer.Write(k)
		writer.WriteByte(equalsByte)
		writer.Write([]byte(*point.Tags[string(k)]))
		tagIndex++
		if tagIndex < tagsCount {
			writer.WriteByte(commaByte)
		}
	}

	writer.WriteByte(spaceByte)
	fieldsCount := len(point.Fields)
	fieldIndex := 0
	for k, v := range point.Fields {
		writer.Write([]byte(k))
		writer.WriteByte(equalsByte)
		writer.Write([]byte(*v))
		fieldIndex++
		if fieldIndex < fieldsCount {
			writer.WriteByte(commaByte)
		}
	}
	writer.WriteByte(spaceByte)
	writer.Write([]byte(strconv.FormatInt(point.Timestamp, 10)))
	if hasMore {
		writer.WriteByte(newlineByte)
	}
}

func SortByteSlices(src [][]byte) [][]byte {
	sorted := sortByteArrays(src)
	sort.Sort(sorted)
	return sorted
}

// implement `Interface` in sort package.
type sortByteArrays [][]byte

func (b sortByteArrays) Len() int {
	return len(b)
}

func (b sortByteArrays) Less(i, j int) bool {
	// bytes package already implements Comparable for []byte.
	switch bytes.Compare(b[i], b[j]) {
	case -1:
		return true
	case 0, 1:
		return false
	default:
		log.Panic("not fail-able with `bytes.Comparable` bounded [-1, 1].")
		return false
	}
}

func (b sortByteArrays) Swap(i, j int) {
	b[j], b[i] = b[i], b[j]
}

func intmin(a, b int) int {
	if a < b {
		return a
	} else {
		return b
	}
}
