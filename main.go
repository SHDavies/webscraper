package main

import (
	"archive/zip"
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
	timeoutThreshold *int
	quiet            *bool
)

func main() {
	// FLAGS
	reqThreads = flag.Int("r", 4, "Number of concurrent requests per page")
	pageThreads := flag.Int("p", 4, "Number of pages to work on concurrently")
	timeoutThreshold = flag.Int("t", 10, "Seconds to allow http requests before aborting")
	quiet = flag.Bool("q", false, "Don't log http requests")
	flag.Parse()

	// Queue for concurrent .txt files
	pageQueue := make(chan struct{}, *pageThreads)
	start := time.Now()
	thisDir, err := ioutil.ReadDir(".")
	if err != nil {
		log.Fatalln(fmt.Errorf("error reading dir: %v", err))
	}

	// Create log file if it doesn't exist
	logFile, err = os.Open("webcrawl.log")
	if err != nil {
		logFile, err = os.Create("webcrawl.log")
		if err != nil {
			log.Fatalln(fmt.Errorf("error creating log file: %v", err))
		}
	}
	logger = log.New(logFile, "ERROR: ", log.LstdFlags)

	// WaitGroup ensures program waits for all goroutines before printing stats and exiting
	var wg sync.WaitGroup

	for _, f := range thisDir {
		wg.Add(1)
		if !f.IsDir() {
			go func(file os.FileInfo) {
				defer wg.Done()
				// Occupy space in queue (blocks if queue is full)
				pageQueue <- struct{}{}
				err = handleFile(file.Name())
				if err != nil {
					fmt.Println(err)
					logger.Println(err)
					totalErrs++
				}
				// Release spot in queue
				<-pageQueue
			}(f)
		} else {
			wg.Done()
		}
	}

	// Wait for all goroutines then print stats and exit
	wg.Wait()
	close(pageQueue)
	fmt.Printf("TOTAL FILES FETCHED: %v\n", total)
	fmt.Printf("TOTAL ERRORS (check webcrawl.log for info): %v\n", totalErrs)
	fmt.Printf("TOTAL TIME: %v\n", time.Since(start))
}

func handleFile(fname string) error {
	fmt.Println("------->", fname)
	// Open .txt file
	file, err := os.Open(fname)
	if err != nil {
		return fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()

	// Make new dir for .html files and index
	name := strings.Split(fname, ".")[0]
	err = os.Mkdir(name, 0755)
	if err != nil {
		return fmt.Errorf("error creating dir %v: %v", name, err)
	}

	// Create index file
	index, err := os.Create(filepath.Join(name, "index.txt"))
	if err != nil {
		return fmt.Errorf("error creating index.txt: %v", err)
	}
	defer index.Close()

	urlCount := 1

	var wg sync.WaitGroup
	var mu sync.Mutex

	// Allows for aborting requests
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}

	// Queue for concurrent http requests
	requestQueue := make(chan struct{}, *reqThreads)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()

			// Ignore pdf files
			if len(strings.TrimSpace(url)) <= 0 || strings.Contains(strings.ToLower(url), "pdf") {
				return
			}

			// Occupy spot in queue
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

					// Prevent race conditions
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

			// Fire off a timeout if request takes 10 seconds
			timeout := make(chan struct{})
			go func() {
				time.Sleep(10 * time.Second)
				timeout <- struct{}{}
			}()

			// Handle timeout or response - whichever happens first
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

			// Release spot in queue
			<-requestQueue
		}(scanner.Text())
	}
	wg.Wait()

	// Create archive of html files
	err = zipit(name, name+".zip")
	if err != nil {
		return fmt.Errorf("error creating zip: %v", err)
	}

	err = os.RemoveAll(name)
	if err != nil {
		return fmt.Errorf("error deleting directory %v: %v", name, err)
	}

	return nil
}

func zipit(source, target string) error {
	zipfile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer zipfile.Close()

	archive := zip.NewWriter(zipfile)
	defer archive.Close()

	info, err := os.Stat(source)
	if err != nil {
		return nil
	}

	var baseDir string
	if info.IsDir() {
		baseDir = filepath.Base(source)
	}

	filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		if baseDir != "" {
			header.Name = filepath.Join(baseDir, strings.TrimPrefix(path, source))
		}

		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})

	return err
}
