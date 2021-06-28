package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
)

func Connect(ctx context.Context, host string, port int, password string) (*redis.Client, error) {
	rdb := redis.NewClient(
		&redis.Options{
			Addr:     fmt.Sprintf("%s:%d", host, port),
			Password: password,
		},
	)
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("redis connect error:%w", err)
	}
	return rdb, nil
}

type countAdaptor int64

func (c *countAdaptor) Update(t time.Duration) {
	if t > time.Millisecond*10 {
		return
	}
	if *c > 1024 {
		*c += 32
		return
	}
	*c *= 2
}

type ScanOption struct {
	Cursor uint64
	Match  string
	Retry  uint64
}

func ScanWithRetry(ctx context.Context, rdb *redis.Client, opt ScanOption,
	handle func(keys []string) bool) error {
	var count countAdaptor = 32
	var keys []string
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context exceed")
		default:
			break
		}
		var err error
		var cost time.Duration
		var cursor uint64
		for i := uint64(0); i < opt.Retry; i++ {
			t := time.Now()
			keys, cursor, err = rdb.Scan(ctx, opt.Cursor, opt.Match, int64(count)).Result()
			if err != nil {
				cost = time.Since(t)
				break
			}
		}
		if err != nil {
			return fmt.Errorf("scan error: %w", err)
		}
		opt.Cursor = cursor
		count.Update(cost)
		if !handle(keys) {
			break
		}
		if cursor == 0 {
			break
		}
	}
	return nil
}

type Stat map[string]uint64

func (s Stat) Update(keys []string, filter func(k string) string) {
	for _, k := range keys {
		r := filter(k)
		s[r] += 1
	}
}

func (s Stat) Print() {
	var keys []string
	for k := range s {
		keys = append(keys, k)
	}
	sort.Sort(sort.StringSlice(keys))
	fmt.Println("#pattern number")
	for _, k := range keys {
		v := s[k]
		fmt.Printf("%s %d\n", k, v)
	}
}

type TemplateData struct {
	Matches []string
}

type Patcher struct {
	Filter string
	Into   string
	tpl    *template.Template
	reg    *regexp.Regexp
}

func NewPatcher(f string, into string) (*Patcher, error) {
	r, err := regexp.Compile(f)
	if err != nil {
		return nil, fmt.Errorf("bad filter: %w", err)
	}
	t, err := parseTemplate(into, r)
	if err != nil {
		return nil, fmt.Errorf("bad template: %w", err)
	}
	v := &Patcher{Filter: f, Into: into, reg: r, tpl: t}
	return v, nil
}

func (f *Patcher) Flit(k string) string {
	r := f.reg.FindStringSubmatch(k)
	if len(r) == 0 {
		return ""
	}
	v := &TemplateData{Matches: r}
	sb := &strings.Builder{}
	f.tpl.Execute(sb, v)
	return sb.String()
}

func parseTemplate(pattern string, r *regexp.Regexp) (*template.Template, error) {
	subExpNames := r.SubexpNames()
	tstr, err := replaceVars(pattern, func(name string) (string, error) {
		index, err := varToIndex(subExpNames, name)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("{{index .Matches %d}}", index), nil
	})
	if err != nil {
		return nil, err
	}
	return template.New("").Parse(tstr)
}

func isEscaped(s string, pos int) bool {
	slashes := 0
	for i := pos - 1; i >= 0; i-- {
		if s[i] == '\\' {
			slashes++
		} else {
			break
		}
	}
	return slashes%2 != 0
}

func varToIndex(subExpNames []string, name string) (int, error) {
	if i, err := strconv.Atoi(name); err == nil {
		if i >= len(subExpNames) {
			return 0, fmt.Errorf("$%s exceed the number of subexpressions", name)
		}
		return i, nil
	}
	for i := 0; i < len(subExpNames); i++ {
		if subExpNames[i] == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("$%s does not correspond to any subexpression", name)
}

func replaceVars(s string, f func(string) (string, error)) (string, error) {
	var (
		regex   = regexp.MustCompile(`\$(:?([\w\d]+)|{([\w\d]+)})`)
		matches = regex.FindAllStringSubmatchIndex(s, -1)
		index   = 0
		buffer  bytes.Buffer
	)
	for _, m := range matches {
		var name string
		if m[4] != -1 {
			name = s[m[4]:m[5]]
		} else {
			name = s[m[6]:m[7]]
		}
		if !isEscaped(s, m[0]) {
			buffer.WriteString(s[index:m[0]])
			if replace, err := f(name); err != nil {
				return "", err
			} else {
				buffer.WriteString(replace)
			}
			index = m[1]
		}
	}
	buffer.WriteString(s[index:])
	return buffer.String(), nil
}

type Opt struct {
	Host     string
	Port     int
	Password string
	Match    string
	Retry    uint64
	Max      uint64
	Pattern  string
	Replace  string
}

func (o *Opt) init() {
	o.Host = "localhost"
	o.Port = 6379
	o.Retry = 10
	o.Max = math.MaxUint64
}

var patternAndReplaceHelp = `
patter is an regex expression and replace is key statistic group result. 
See an example of group keys by it's prefix:
pattern is '(.*)_.*' and replace is '$1'`

func (o *Opt) parseCommandLine() {
	o.init()
	name := filepath.Base(os.Args[0])
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [Options...] pattern replace\n", name)
		fmt.Fprintf(flag.CommandLine.Output(), patternAndReplaceHelp + "\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.StringVar(&o.Host, "h", o.Host, "Server hostname")
	flag.IntVar(&o.Port, "p", o.Port, "Server port")
	flag.StringVar(&o.Match, "m", o.Match, "Redis scan match pattern")
	flag.StringVar(&o.Password, "a", o.Password, "Password to use when connecting to the server")
	flag.Uint64Var(&o.Retry, "r", o.Retry, "Retry times")
	flag.Uint64Var(&o.Max, "max", o.Max, "Max keys to scan")

	flag.Parse()
	o.Pattern = flag.Arg(0)
	o.Replace = flag.Arg(1)
	if o.Pattern == "" || o.Replace == "" {
		fmt.Println("pattern or replace required")
		flag.Usage()
		os.Exit(1)
	}
}

func fatalTest(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func main() {
	var o Opt
	o.parseCommandLine()

	ctx, cancel := context.WithCancel(context.Background())
	rdb, err := Connect(ctx, o.Host, o.Port, o.Password)
	fatalTest(err)
	f, err := NewPatcher(o.Pattern, o.Replace)
	fatalTest(err)

	scanopts := ScanOption{
		Match: o.Match,
		Retry: o.Retry,
	}
	stats := make(Stat)
	var total uint64
	filter := func(keys []string) bool {
		stats.Update(keys, f.Flit)
		total += uint64(len(keys))
		if total > o.Max {
			return false
		}
		return len(keys) != 0
	}
	sigs := make(chan os.Signal, 10)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			<-sigs
			fmt.Println("wait cancel")
			cancel()
		}
	}()

	start := time.Now()
	err = ScanWithRetry(ctx, rdb, scanopts, filter)
	stats.Print()
	fmt.Printf("\nscan keys: %d, spend: %s\n", total, time.Since(start))
	fatalTest(err)
}