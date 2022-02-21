package main

import (
	"bufio"
	"crypto/tls"
	"flag"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
)

// https://zhwt.github.io/yaml-to-go/
//type Config struct {
//	Rules []struct {
//		Name			 string	 `yaml:"name"`
//		Tokens		 []string `yaml:"tokens"`
//		Processors []struct {
//			Urlencode		string `yaml:"urlencode,omitempty"`
//			Base64Encode string `yaml:"base64encode,omitempty"`
//		} `yaml:"processors"`
//		Inquisitors []string `yaml:"inquisitors"`
//	} `yaml:"rules"`
//}

type Config struct {
	Rules []*Rule `yaml:"rules"`
}
type Rule struct {
	Name        string `yaml:"name"`
	Filepath    string
	Tokens      []string     `yaml:"tokens"`
	Processors  []*Processor `yaml:"processors"`
	Inquisitors []string     `yaml:"inquisitors"`
}
type Processor struct {
	Urlencode    string `yaml:"urlencode,omitempty"`
	Base64Encode string `yaml:"base64encode,omitempty"`
}
type URL struct {
	Value   string
	Domain  string
	IPAddrs []net.IP
	CName   string

	// Obtained from http.Get
	StatusCode    int
	ContentLength int64
	Protocol      string // "HTTP/1.0"

	// Example tokens: port, path, fragment, queryparams, ...
	Tokens map[string]string
	//Inquisitors []string
}

// Equality between two URLs depends on the Rule struct
func (u *URL) equals(u2 *URL, rule *Rule) bool {
	// Short-circuit test: Compare Values
	if u.Value == u2.Value {
		return true
	}

	// Compare Tokens
	for _, element := range rule.Tokens {
		needleToken, _ := u.Tokens[element]
		haystackToken, _ := u2.Tokens[element]
		if needleToken != haystackToken {
			return false
		}
	}

	// Compare Processors

	// Compare Inquisitors
	for _, element := range rule.Inquisitors {
		switch element {
		case "dnsa":
			matchExists := false
			for _, uIP := range u.IPAddrs {
				for _, u2IP := range u2.IPAddrs {
					if uIP.Equal(u2IP) {
						matchExists = true
					}
				}
			}
			if !matchExists {
				return false
			}
		case "dnscname":
			if u.CName != u2.CName {
				return false
			}
		case "statuscode":
			if u.StatusCode != u2.StatusCode {
				return false
			}
		case "contentlength":
			if u.ContentLength != u2.ContentLength {
				return false
			}
		default:
			fmt.Println("Unknown Inquisitor: " + element)
		}
	}

	// By this point, all definitions of equality have passed
	return true
}

func parseURL(input string) *URL {
	u, err := url.Parse(input)
	if err != nil {
		panic(err)
	}

	domain := u.Hostname()
	tokens := make(map[string]string)
	tokens["scheme"] = u.Scheme
	tokens["username"] = u.User.Username()
	tokens["password"], _ = u.User.Password()
	tokens["domain"] = domain
	tokens["port"] = u.Port()
	tokens["path"] = u.Path
	tokens["fragment"] = u.Fragment
	tokens["queryparams"] = u.RawQuery

	ret := URL{
		Tokens: tokens,
		Domain: domain,
		Value:  input}
	return &ret
}

func existsWithin(needle *URL, haystack *[]URL, rules *[]Rule) bool {
	// Compare given needle to every item in haystack
	for _, url := range *haystack {
		for _, rule := range *rules {
			if needle.equals(&url, &rule) {
				if verbose {
					log.Print("[+] Duplicate found!")
					log.Print("[+]     Omitting:     " + needle.Value)
					log.Print("[+]     Duplicate of: " + url.Value)
					log.Print("[+]     Per rule:     " + rule.Name + " (" + rule.Filepath + ")")
				}
				return true
			}
		}
	}
	return false
}

var verbose bool

func main() {
	rulesFilepath := flag.String("rules", "rules/default.yml", "Filepath for rule configuration")
	ruleName := flag.String("rule", "*", "The single named rule to use (as defined in the rule configuration file)")
	inputFilepath := flag.String("input", "input.txt", "Filepath for list of URLs")
	var insecure bool
	flag.BoolVar(&insecure, "insecure", false, "Disable TLS certificate verification")
	flag.BoolVar(&verbose, "verbose", false, "Increase verbosity in stderr")
	//cTimeout := flag.Int("timeout", 5, "Connection timeout in seconds")
	flag.Parse()

	// Set log flags. Available flags: https://pkg.go.dev/log#pkg-constants
	//log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetFlags(0)

	// Parse configuration into struct
	rulesFile, err := ioutil.ReadFile(*rulesFilepath)
	if err != nil {
		fmt.Println(err)
	}
	var cfg Config
	err = yaml.Unmarshal(rulesFile, &cfg)
	if err != nil {
		fmt.Println(err)
	}

	// Find the configuration rules to use
	var rules []Rule
	for _, element := range cfg.Rules {
		if element.Name == *ruleName {
			element.Filepath = *rulesFilepath
			rules = append(rules, *element)
		} else if *ruleName == "*" {
			element.Filepath = *rulesFilepath
			rules = append(rules, *element)
		}
	}
	if len(rules) == 0 {
		fmt.Println("Uh oh, looks like the rule \"" + *ruleName + "\" was not found!")
	}

	// Get the processor type
	//val := reflect.Indirect(reflect.ValueOf(rule.Processors[0]))
	//fmt.Println(val.Type().Field(0).Name)

	// Parse input file into array of URLs
	var urls []URL
	f, err := os.Open(*inputFilepath)
	if err != nil {
		fmt.Println(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// Note: scanner limits lines to 64kb.
	for scanner.Scan() {
		urls = append(urls, *parseURL(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		fmt.Println(err)
	}

	// Prepopulate Inquisitor-based URL attributes (fetch data only if the config rules need it)
	// Booleans used to save whether URL struct has populated the data (to prevent multiple fetches)
	isPopulatedDNSA := false
	isPopulatedDNSCNAME := false
	isPopulatedSCCL := false

	// Create HTTP client
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	httpClient := &http.Client{Transport: tr}

	for _, rule := range rules {
		for _, element := range rule.Inquisitors {
			switch element {
			case "dnsa":
				if !isPopulatedDNSA {
					if verbose {
						log.Print("[+] Performing DNS A-Record queries...")
					}
					for i := 0; i < len(urls); i++ {
						if len(urls[i].IPAddrs) == 0 {
							urls[i].IPAddrs, _ = net.LookupIP(urls[i].Domain)
						}
					}
				}
				isPopulatedDNSA = true
			case "dnscname":
				if !isPopulatedDNSCNAME {
					if verbose {
						log.Print("[+] Performing DNS CNAME-Record queries...")
					}
					for i := 0; i < len(urls); i++ {
						if urls[i].CName == "" {
							urls[i].CName, _ = net.LookupCNAME(urls[i].Domain)
						}
					}
				}
				isPopulatedDNSCNAME = true
			// If there's any of the http-get Inquisitors, populate them all from one query:
			case "statuscode", "contentlength":
				if !isPopulatedSCCL {
					if verbose {
						log.Print("[+] Performing HTTP queries...")
					}
					for i := 0; i < len(urls); i++ {
						if urls[i].StatusCode == 0 {
							resp, err := httpClient.Get(urls[i].Value)
							if err != nil {
								fmt.Println("ERROR: " + err.Error())
							}
							if resp != nil {
								urls[i].StatusCode = resp.StatusCode
								urls[i].ContentLength = resp.ContentLength
							}
						}
					}
				}
				isPopulatedSCCL = true
			}
		}
	}

	// Get uniques
	var urlsUnique []URL
	for _, element := range urls {
		if !existsWithin(&element, &urlsUnique, &rules) {
			urlsUnique = append(urlsUnique, element)
		} else {
			//fmt.Println("Not unique!")
		}
	}

	// Output results
	if verbose {
		log.Print("[+] Input list length:  " + fmt.Sprintf("%d", len(urls)))
		log.Print("[+] Output list length: " + fmt.Sprintf("%d", len(urlsUnique)))
		log.Print("[+] Printing list of unique URLs...")
	}
	for _, element := range urlsUnique {
		fmt.Println(element.Value)
	}
}
