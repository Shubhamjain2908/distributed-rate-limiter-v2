package middleware

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RouteCost holds the YAML map and optional file default. Nil or empty m means route-based costs disabled.
type RouteCost struct {
	m   map[string]int
	def int
}

type routeCostFile struct {
	Routes  map[string]int `yaml:"routes"`
	Default *int           `yaml:"default"`
}

// LoadRouteCost reads path; missing file => (nil, nil), parse error => err.
func LoadRouteCost(path string) (*RouteCost, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f routeCostFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("route cost %s: %w", path, err)
	}
	rc := &RouteCost{m: f.Routes}
	if f.Default != nil {
		rc.def = *f.Default
	}
	if rc.m == nil {
		rc.m = make(map[string]int)
	}
	return rc, nil
}

// ResolveCost: config route, then X-Request-Cost, then CostHeader (e.g. X-Rate-Limit-Cost), then YAML default, then opts.DefaultCost. Pass-through to limiter and Redis ARGV[4] as "requested".
func ResolveCost(r *http.Request, o *Options) int {
	if o.RouteCost != nil && o.RouteCost.m != nil {
		k := strings.ToUpper(r.Method) + " " + r.URL.Path
		if c, ok := o.RouteCost.m[k]; ok {
			return c
		}
	}
	if h := strings.TrimSpace(r.Header.Get(HeaderXRequestCost)); h != "" {
		if c, e := strconv.Atoi(h); e == nil {
			return c
		}
	}
	if o.CostHeader != "" {
		if h := strings.TrimSpace(r.Header.Get(o.CostHeader)); h != "" {
			if c, e := strconv.Atoi(h); e == nil {
				return c
			}
		}
	}
	if o.RouteCost != nil && o.RouteCost.def > 0 {
		return o.RouteCost.def
	}
	if o.DefaultCost < 0 {
		return 0
	}
	return o.DefaultCost
}
