// Copyright (C) 2020 Gregory Anders
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/kyoh86/xdg"
	"github.com/rivo/tview"
)

var Version string

type Options struct {
	compact     bool
	nullInput   bool
	slurp       bool
	rawOutput   bool
	rawInput    bool
	monochrome  bool
	sortKeys    bool
	historyFile string
}

func contains(arr []string, elem string) bool {
	for _, v := range arr {
		if elem == v {
			return true
		}
	}

	return false
}

func (o *Options) ToSlice() []string {
	opts := []string{}
	if o.compact {
		opts = append(opts, "-c")
	}

	if o.nullInput {
		opts = append(opts, "-n")
	}

	if o.slurp {
		opts = append(opts, "-s")
	}

	if o.rawOutput {
		opts = append(opts, "-r")
	}

	if o.rawInput {
		opts = append(opts, "-R")
	}

	if !o.monochrome {
		opts = append(opts, "-C")
	}

	if o.sortKeys {
		opts = append(opts, "-S")
	}

	return opts
}

func stdinHasData() bool {
	stat, _ := os.Stdin.Stat()
	return stat.Mode()&os.ModeCharDevice == 0
}

type Document struct {
	input   string
	options Options
}

func (d *Document) FromFile(filename string) error {
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	d.input = string(bytes)
	return nil
}

func (d *Document) FromStdin() error {
	if !stdinHasData() {
		// stdin is not being piped
		return errors.New("no data on stdin")
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(os.Stdin); err != nil {
		return err
	}

	d.input = buf.String()

	return nil
}

func (d *Document) Read(args []string) error {
	if d.options.nullInput {
		return nil
	}

	if len(args) > 0 {
		for _, file := range args {
			if err := d.FromFile(file); err != nil {
				return err
			}
		}
	} else {
		if err := d.FromStdin(); err != nil {
			return err
		}
	}

	return nil
}

func (d *Document) Filter(filter string) (string, error) {
	args := append(d.options.ToSlice(), filter)
	cmd := exec.Command("jq", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}

	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, d.input)
	}()

	out, err := cmd.CombinedOutput()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			// jq prints its error message to standard out, but we
			// will deliver it in the Stderr field as this will
			// most likely be an exec.ExitError.
			exiterr.Stderr = out
		}
		return "", err
	}

	return string(out), nil

}

func appendToFile(filepath, line string) error {
	if filepath == "" {
		return errors.New("no filepath specified")
	}

	file, err := os.OpenFile(filepath, (os.O_APPEND | os.O_CREATE | os.O_WRONLY), 0644)
	if err != nil {
		return err
	}

	if _, err := file.WriteString(line + "\n"); err != nil {
		return err
	}

	if err = file.Close(); err != nil {
		return err
	}

	return nil
}

func readFromFile(filepath string) ([]string, error) {
	f, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

func parseArgs() (Options, string, []string) {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ijq - interactive jq\n\n")
		fmt.Fprintf(os.Stderr, "Usage: ijq [-cnsrRMSV] [-f file] [filter] [files ...]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	options := Options{}
	flag.BoolVar(&options.compact, "c", false, "compact instead of pretty-printed output")
	flag.BoolVar(&options.nullInput, "n", false, "use ```null` as the single input value")
	flag.BoolVar(&options.slurp, "s", false, "read (slurp) all inputs into an array; apply filter to it")
	flag.BoolVar(&options.rawOutput, "r", false, "output raw strings, not JSON texts")
	flag.BoolVar(&options.rawInput, "R", false, "read raw strings, not JSON texts")
	flag.BoolVar(&options.monochrome, "M", false, "don't colorize JSON")
	flag.BoolVar(&options.sortKeys, "S", false, "sort keys of objects on output")

	flag.StringVar(
		&options.historyFile,
		"H",
		filepath.Join(xdg.DataHome(), "ijq", "history"),
		"set path to history file. Set to '' to disable history.",
	)

	filterFile := flag.String("f", "", "read initial filter from `filename`")
	version := flag.Bool("V", false, "print version and exit")

	flag.Parse()

	if *version {
		fmt.Println("ijq " + Version)
		os.Exit(0)
	}

	filter := "."
	args := flag.Args()

	if *filterFile != "" {
		contents, err := ioutil.ReadFile(*filterFile)
		if err != nil {
			log.Fatalln(err)
		}

		filter = string(contents)
	} else if len(args) > 1 || (len(args) > 0 && (stdinHasData() || options.nullInput)) {
		filter = args[0]
		args = args[1:]
	} else if len(args) == 0 && !stdinHasData() && !options.nullInput {
		flag.Usage()
		os.Exit(1)
	}

	_ = os.MkdirAll(filepath.Dir(options.historyFile), os.ModePerm)

	return options, filter, args
}

func createApp(doc Document, filter string) *tview.Application {
	app := tview.NewApplication()

	inputView := tview.NewTextView().SetDynamicColors(true)
	inputView.SetTitle("Input").SetBorder(true)

	outputView := tview.NewTextView().
		SetDynamicColors(true).
		SetChangedFunc(func() {
			app.Draw()
		})

	outputView.SetTitle("Output").SetBorder(true)

	errorView := tview.NewTextView().SetDynamicColors(true)
	errorView.SetTitle("Error").SetBorder(true)

	errorWriter := tview.ANSIWriter(errorView)
	outputWriter := tview.ANSIWriter(outputView)

	// If reading the history file fails then just ignore the error and
	// move on
	history, _ := readFromFile(doc.options.historyFile)

	var mutex sync.Mutex
	filterMap := make(map[string][]string)
	filterInput := tview.NewInputField()
	filterInput.
		SetText(filter).
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetFieldTextColor(tcell.ColorSilver).
		SetChangedFunc(func(text string) {
			go app.QueueUpdateDraw(func() {
				errorView.Clear()
				out, err := doc.Filter(text)
				if err != nil {
					filterInput.SetFieldTextColor(tcell.ColorMaroon)
					exitErr, ok := err.(*exec.ExitError)
					if ok {
						fmt.Fprint(errorWriter, string(exitErr.Stderr))
					}

					return
				}

				filterInput.SetFieldTextColor(tcell.ColorSilver)
				outputView.Clear()
				fmt.Fprint(outputWriter, out)
				outputView.ScrollToBeginning()
			})
		}).
		SetDoneFunc(func(key tcell.Key) {
			switch key {
			case tcell.KeyEnter:
				app.Stop()
				expression := filterInput.GetText()
				output := outputView.GetText(true)
				fmt.Fprintln(os.Stderr, expression)
				fmt.Fprint(os.Stdout, output)

				if expression != "" && !contains(history, expression) {
					_ = appendToFile(doc.options.historyFile, expression)
				}
			}
		}).
		SetAutocompleteFunc(func(text string) []string {
			if filterInput.GetText() == "" && len(history) > 0 {
				return history
			}

			if pos := strings.LastIndexByte(text, '.'); pos != -1 {
				prefix := text[0:pos]

				mutex.Lock()
				defer mutex.Unlock()
				entries, ok := filterMap[prefix]
				if ok {
					return entries
				}

				go func() {
					var filt string
					if prefix != "" {
						filt = prefix + "| keys"
					} else {
						filt = "keys"
					}

					d := Document{input: doc.input, options: Options{monochrome: true}}
					out, err := d.Filter("[" + filt + "] | unique | first")
					if err != nil {
						return
					}

					var keys []string
					if err := json.Unmarshal([]byte(out), &keys); err != nil {
						return
					}

					entries := keys[:0]
					for _, k := range keys {
						entries = append(entries, prefix+"."+k)
					}

					mutex.Lock()
					filterMap[prefix] = entries
					mutex.Unlock()

					filterInput.Autocomplete()

					app.Draw()
				}()
			}

			return nil
		})

	filterInput.SetTitle("Filter").SetBorder(true)

	// Filter output with original filter
	go func() {
		orig, err := doc.Filter(".")
		if err != nil {
			log.Fatalln(err)
		}

		out, err := doc.Filter(filter)
		if err != nil {
			filterInput.SetFieldTextColor(tcell.ColorMaroon)
		}

		fmt.Fprint(tview.ANSIWriter(inputView), orig)
		fmt.Fprint(outputWriter, out)
	}()

	grid := tview.NewGrid().
		SetRows(0, 3, 4).
		SetColumns(0).
		AddItem(tview.NewFlex().
			AddItem(inputView, 0, 1, false).
			AddItem(outputView, 0, 1, false), 0, 0, 1, 1, 0, 0, false).
		AddItem(tview.NewFlex().
			AddItem(tview.NewBox(), 0, 1, false).
			AddItem(filterInput, 0, 4, true).
			AddItem(tview.NewBox(), 0, 1, false), 1, 0, 1, 1, 0, 0, true).
		AddItem(tview.NewFlex().
			AddItem(tview.NewBox(), 0, 1, false).
			AddItem(errorView, 0, 4, false).
			AddItem(tview.NewBox(), 0, 1, false), 2, 0, 1, 1, 0, 0, false)

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		shift := event.Modifiers()&tcell.ModShift != 0
		switch key := event.Key(); key {
		case tcell.KeyCtrlN:
			return tcell.NewEventKey(tcell.KeyDown, ' ', tcell.ModNone)
		case tcell.KeyCtrlP:
			return tcell.NewEventKey(tcell.KeyUp, ' ', tcell.ModNone)
		case tcell.KeyUp:
			if shift && filterInput.HasFocus() {
				app.SetFocus(inputView)
				return nil
			}
		case tcell.KeyDown:
			if shift {
				app.SetFocus(filterInput)
				return nil
			}
		case tcell.KeyLeft:
			if outputView.HasFocus() {
				app.SetFocus(inputView)
				return nil
			}
		case tcell.KeyRight:
			if inputView.HasFocus() {
				app.SetFocus(outputView)
				return nil
			}
		}

		return event
	})

	app.SetRoot(grid, true).SetFocus(grid)

	return app
}

func main() {
	// Remove log prefix
	log.SetFlags(0)

	options, filter, args := parseArgs()

	doc := Document{options: options}
	if err := doc.Read(args); err != nil {
		log.Fatalln(err)
	}

	app := createApp(doc, filter)
	if err := app.Run(); err != nil {
		log.Fatalln(err)
	}
}
