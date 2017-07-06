package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	reqThreads       *int
	logger           *log.Logger
	logFile          *os.File
	total            int
	totalErrs        int
	wd               string
	timeoutThreshold *int
	quiet            *bool
)

func main() {
	reqThreads = flag.Int("r", 4, "Number of concurrent requests per page")
	pageThreads := flag.Int("p", 4, "Number of pages to work on concurrently")
	timeoutThreshold = flag.Int("t", 10, "Seconds to allow http requests before aborting")
	quiet = flag.Bool("q", false, "Don't log http requests")
	flag.Parse()
	pageQueue := make(chan struct{}, *pageThreads)
	start := time.Now()
	thisDir, err := ioutil.ReadDir(".")
	if err != nil {
		log.Fatalln(fmt.Errorf("error reading dir: %v", err))
	}
	logFile, err = os.Open("webcrawl.log")
	if err != nil {
		logFile, err = os.Create("webcrawl.log")
		if err != nil {
			log.Fatalln(fmt.Errorf("error creating log file: %v", err))
		}
	}
	logger = log.New(logFile, "ERROR: ", log.LstdFlags)
	wd, err = os.Getwd()
	if err != nil {
		logger.Fatalln(err)
	}
	var wg sync.WaitGroup
	for _, f := range thisDir {
		wg.Add(1)
		if !f.IsDir() {
			go func(file os.FileInfo) {
				pageQueue <- struct{}{}
				fmt.Println(file.Name())
				err = handleFile(file.Name())
				if err != nil {
					fmt.Println(err)
					logger.Println(err)
					totalErrs++
				}
				wg.Done()
				<-pageQueue
			}(f)
		} else {
			wg.Done()
		}
	}
	wg.Wait()
	fmt.Printf("TOTAL FILES FETCHED: %v\n", total)
	fmt.Printf("TOTAL ERRORS (check webcrawl.log for info): %v\n", totalErrs)
	fmt.Printf("TOTAL TIME: %v\n", time.Since(start))
}

func handleFile(fname string) error {
	file, err := os.Open(fname)
	if err != nil {
		return fmt.Errorf("error opening file: %v", err)
	}
	name := strings.Split(fname, ".")[0]
	err = os.Mkdir(name, 0755)
	if err != nil {
		return err
	}
	index, err := os.Create(filepath.Join(name, "index.txt"))
	if err != nil {
		return fmt.Errorf("error creating index.txt: %v", err)
	}
	defer index.Close()
	urlCount := 1
	var wg sync.WaitGroup
	var mu sync.Mutex
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	requestQueue := make(chan struct{}, *reqThreads)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			if len(strings.TrimSpace(url)) <= 0 || strings.Contains(url, ".pdf") {
				return
			}
			requestQueue <- struct{}{}
			if !*quiet {
				fmt.Printf("Getting %v\n", url)
			}
			request, err := http.NewRequest("GET", url, nil)
			if err != nil {
				logger.Println(err)
				totalErrs++
				return
			}
			c := make(chan error, 1)
			go func() {
				resp, err := client.Do(request)
				if err == nil {
					defer resp.Body.Close()
					mu.Lock()
					newFileName := fmt.Sprint(urlCount) + ".html"
					urlCount++
					total++
					mu.Unlock()
					newFile, err := os.Create(filepath.Join(name, newFileName))
					if err != nil {
						logger.Println(err)
						totalErrs++
						return
					}
					io.Copy(newFile, resp.Body)
					newFile.Close()
					index.WriteString(fmt.Sprintf("%v, %v\n", url, newFile.Name()))
				}
				c <- err
			}()
			timeout := make(chan struct{})
			go func() {
				time.Sleep(10 * time.Second)
				timeout <- struct{}{}
			}()
			select {
			case <-timeout:
				logger.Println(fmt.Errorf("timed out: %v", url))
				fmt.Println("Aborting", url)
				tr.CancelRequest(request)
			case err = <-c:
				if err != nil {
					logger.Println(err)
					totalErrs++
				}
			}
			<-requestQueue
		}(scanner.Text())
	}
	wg.Wait()
	return nil
}
