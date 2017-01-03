package main

// TODO: batch API
// TODO: configure size of sketches (width/depth/window)
// TODO: expvar, net/http/pprof
// TODO: graphite?
// TODO: logging
// TODO: persist/reload hokusai data
// TODO: support multiple (dynamic?) named sketches: /create?name=stream1&...

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgryski/hokusai/sketch"
)

type hoku struct {
	sketch *sketch.Hokusai
	sync.Mutex
}

func (h *hoku) Add(epoch int64, x string, count uint32) {
	h.Lock()
	h.sketch.Add(epoch, x, count)
	h.Unlock()
}

func (h *hoku) Count(epoch int64, x string) uint32 {
	h.Lock()
	count := h.sketch.Count(epoch, x)
	h.Unlock()
	return count
}

var Hoku hoku

var Epoch0 int64

var WindowSize int64 = 60

func defaultInt(s string, defval int) (int, error) {
	if s == "" {
		return defval, nil
	}

	sint, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}

	return sint, nil
}

func addHandler(w http.ResponseWriter, r *http.Request) {

	item := r.FormValue("item")

	count, err := defaultInt(r.FormValue("count"), 1)
	if err != nil {
		http.Error(w, "bad count", http.StatusBadRequest)
		return
	}

	targ, err := defaultInt(r.FormValue("t"), 0)
	if err != nil {
		http.Error(w, "bad epoch", http.StatusBadRequest)
		return
	}

	var epoch int64
	if targ == 0 {
		epoch = time.Now().Unix()
	} else {
		epoch = int64(targ)
	}

	Hoku.Add(epoch, item, uint32(count))
	TopKs.Insert(item, count)

	// TODO: more interesting response?
	fmt.Fprintln(w, "Ok", epoch)
}

type QueryResponse struct {
	Query  string
	Start  int
	Step   int64
	Counts []uint32
}

func queryHandler(w http.ResponseWriter, r *http.Request) {

	q := r.FormValue("q")

	start, err := defaultInt(r.FormValue("start"), 0)
	if start == 0 || int64(start) < Epoch0 || err != nil {
		http.Error(w, "bad start epoch", http.StatusBadRequest)
		return
	}

	stop, err := defaultInt(r.FormValue("stop"), 0)
	if stop == 0 || int64(stop) < Epoch0 || err != nil {
		http.Error(w, "bad stop epoch", http.StatusBadRequest)
		return
	}

	queryResponse := QueryResponse{
		Query: q,
		Start: start,
		Step:  WindowSize,
	}

	for t := start; t <= stop; t += int(WindowSize) {
		queryResponse.Counts = append(queryResponse.Counts, Hoku.Count(int64(t), q))
	}

	if r.FormValue("format") == "html" {
		reportTmpl.Execute(w, queryResponse)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	jenc := json.NewEncoder(w)
	jenc.Encode(queryResponse)
}

var reportTmpl = template.Must(template.New("report").Parse(`
<html>
<script src="//cdnjs.cloudflare.com/ajax/libs/jquery/2.0.3/jquery.min.js"></script>
<script src="//cdnjs.cloudflare.com/ajax/libs/flot/0.8.2/jquery.flot.min.js"></script>
<script src="//cdnjs.cloudflare.com/ajax/libs/flot/0.8.2/jquery.flot.time.min.js"></script>

<script type="text/javascript">

    var counts = {{ .Counts }};
    var data = []
    for (var i in counts) { data[i] = [({{ .Start }} + i* {{ .Step }}) * 1000,counts[i]]; }

    $(document).ready(function() {
        $.plot($("#placeholder"), [data], {
            xaxis: { mode: "time" }
        })
    })

</script>

<body>

<div id="placeholder" style="width:1200px; height:400px"></div>

</body>
</html>
`))

func topkHandler(w http.ResponseWriter, r *http.Request) {

	epoch, err := defaultInt(r.FormValue("epoch"), -1)
	if epoch < 0 || int64(epoch) < Epoch0 || err != nil {
		http.Error(w, "bad epoch", http.StatusBadRequest)
		return
	}

	// TODO(dgryski): racey once we move to a ring-buffer?
	t := (int64(epoch) - Epoch0) / WindowSize
	if t < 0 || t >= int64(TopKs.Len()) {
		http.Error(w, "bad epoch", http.StatusBadRequest)
		return
	}
	response := TopKs.Keys(int(t))

	w.Header().Set("Content-Type", "application/json")
	jenc := json.NewEncoder(w)
	jenc.Encode(response)
}

func main() {

	epoch0 := flag.Int("epoch", 0, "start epoch (from file)")
	file := flag.String("f", "", "load data from file (instead of http)")
	port := flag.Int("p", 8080, "http port")
	width := flag.Int("w", 20, "default sketch width")
	depth := flag.Int("d", 5, "default sketch depth")
	win := flag.Int("win", 60, "default window size")
	intv := flag.Int("intv", 6, "intervals to keep")
	topks := flag.Int("topk", 100, "topk elements to track")

	flag.Parse()

	WindowSize = int64(*win)

	if *file != "" {
		loadDataFrom(*file, int64(*epoch0), uint(*intv), *width, *depth, *topks)
	} else {

		now := time.Now().Unix()
		Epoch0 = now - (now % int64(WindowSize))
		log.Println("Epoch0=", Epoch0)

		Hoku.sketch = sketch.NewHokusai(Epoch0, int64(WindowSize), uint(*intv), *width, *depth)
		TopKs.Tick(*topks)
		go func() {
			for {
				time.Sleep(time.Second * time.Duration(WindowSize))
				t := time.Now().Unix()
				Hoku.Add(t, "", 0)
				TopKs.Tick(*topks)
			}
		}()
	}

	http.HandleFunc("/add", addHandler)
	http.HandleFunc("/query", queryHandler)
	http.HandleFunc("/topk", topkHandler)

	log.Println("listening on port", *port)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(*port), nil))
}

func loadDataFrom(file string, epoch0 int64, intervals uint, width, depth, topks int) {

	f, err := os.Open(file)
	if err != nil {
		log.Fatal(err)
	}

	scanner := bufio.NewScanner(f)

	Epoch0 = epoch0

	Hoku.sketch = sketch.NewHokusai(epoch0, int64(WindowSize), intervals, width, depth)
	TopKs.Tick(topks)

	maxEpoch := int(epoch0)
	var window int64

	var lines int

	for scanner.Scan() {
		line := scanner.Text()
		lines++
		fields := strings.Split(line, "\t")

		t, err := strconv.Atoi(fields[0])
		if err != nil {
			log.Println("skipping ", fields[0])
			continue
		}

		if t > maxEpoch {
			step := int64(t - maxEpoch)
			maxEpoch = t
			window += step
			if window >= WindowSize {
				window = 0
				TopKs.Tick(topks)
			}
		}

		if lines%(1<<20) == 0 {
			log.Println("processed", lines)
		}

		var count uint32 = 1

		if len(fields) == 3 {
			cint, err := strconv.Atoi(fields[2])
			if err != nil {
				log.Println("failed to parse count: ", fields[2], ":", err)
				continue
			}
			count = uint32(cint)
		}

		Hoku.Add(int64(t), fields[1], count)
		TopKs.Insert(fields[1], int(count))
	}
}
