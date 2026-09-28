// NVENCForgeGUI (https://github.com/burnersen/NVENCForgeGUI)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

// runner.go — einen Konverter-Lauf starten, mitlesen und sauber beenden.
//
// ab-av1 schreibt seine Fortschrittsanzeige auf den Fehlerkanal (stderr), die
// eigentliche Ausgabe auf den Hauptkanal. Beide Leser behandeln ihre Zeilen
// gleich: Was sich als JSON-Ereignis lesen lässt (z.B. das "Progress"-Objekt
// von clap/indicatif), ist eines — alles andere ist Protokoll und wird
// genauso aufbereitet. So funktioniert der Datenkanal auch, falls eine exe
// zusätzlich strukturierte Ereignisse liefert.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// maxEventLine begrenzt eine einzelne Zeile. Dateinamen können lang sein,
// deshalb großzügig — aber nicht unbegrenzt, damit eine kaputte Gegenseite den
// Speicher nicht vollschreiben kann.
const maxEventLine = 1 << 20

var (
	// cursorUpPattern erkennt "gehe n Zeilen hoch". Genau damit zeichnet die
	// Fortschrittsanzeige sich neu: Sie setzt den Cursor zurück und schreibt
	// ihre Zeilen ein weiteres Mal. Ohne diese Auswertung würde das Protokoll
	// pro Sekunde um zehn fast gleiche Zeilen wachsen.
	cursorUpPattern = regexp.MustCompile(`\x1b\[([0-9]*)[AF]`)

	// nonColorEscape entfernt alle Steuerbefehle AUSSER den Farben: Cursor
	// bewegen, Zeile löschen, Fenstertitel setzen. Der einzige Endbuchstabe,
	// der stehen bleibt, ist "m" — das sind die Farben.
	nonColorEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[@-ln-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

	// colorEscape dient nur zum Prüfen, ob nach Abzug der Farben überhaupt
	// noch etwas Sichtbares übrig bleibt.
	colorEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// LogLine ist eine Zeile für das Protokollfenster.
//
// Back sagt, wie viele bereits angezeigte Zeilen diese hier ersetzt. So
// verhält sich das Protokoll wie ein echtes Terminal: Die Fortschrittsanzeige
// überschreibt sich selbst, statt sich zu stapeln.
type LogLine struct {
	Text string `json:"text"`
	Back int    `json:"back"`
	Slot int    `json:"slot"`
}

// RunState beschreibt, was gerade läuft — und wie ein Lauf ausgegangen ist.
type RunState struct {
	Running  bool   `json:"running"`
	Stopping bool   `json:"stopping"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error"`
	Slot     int    `json:"slot"`
}

// Runner überwacht genau einen Konverter-Prozess.
//
// slot ist die Platznummer dieses Läufers (1, 2, 3). Sie hängt an JEDER
// Meldung, die er weitergibt: Laufen zwei Konverter gleichzeitig, wäre sonst
// nicht zu erkennen, welcher Fortschritt und welche Rückfrage zu welcher Datei
// gehört — und eine Antwort landete womöglich beim falschen Prozess.
type Runner struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	pid      int
	slot     int
	stopping bool
	input    io.WriteCloser
	sink     func(name string, data ...any)
}

// NewRunner erzeugt den Läufer für einen Platz. sink reicht Meldungen weiter.
func NewRunner(slot int, sink func(name string, data ...any)) *Runner {
	return &Runner{slot: slot, sink: sink}
}

// emit hängt die Platznummer an und gibt die Meldung weiter. Alles im Läufer
// meldet über diesen einen Weg, damit keine Meldung ohne Nummer entwischt.
func (r *Runner) emit(name string, data ...any) {
	if len(data) == 1 {
		switch payload := data[0].(type) {
		case RunState:
			payload.Slot = r.slot
			r.sink(name, payload)
			return
		case LogLine:
			payload.Slot = r.slot
			r.sink(name, payload)
			return
		case map[string]any:
			payload["slot"] = r.slot
			r.sink(name, payload)
			return
		}
	}
	r.sink(name, data...)
}

// Slot liefert die Platznummer dieses Läufers.
func (r *Runner) Slot() int { return r.slot }

// Running meldet, ob gerade ein Lauf aktiv ist.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil
}

// Start startet den Konverter und beginnt, seine beiden Kanäle mitzulesen.
func (r *Runner) Start(exePath, workDir string, args []string) error {
	r.mu.Lock()
	if r.cmd != nil {
		r.mu.Unlock()
		return errors.New("a conversion is already running")
	}

	cmd := exec.Command(exePath, args...)
	cmd.Dir = workDir
	// Eigene Prozessgruppe: nur so trifft das Abbruch-Signal später genau
	// diesen Lauf. CREATE_NO_WINDOW darf hier NICHT dazu — siehe wincon.go.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("runner.go: Start (StdoutPipe): %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("runner.go: Start (StderrPipe): %w", err)
	}
	// Der Rückweg. Ohne ihn erbt der Konverter keine lesbare Eingabe und
	// beantwortet seine eigene Spurauswahl sofort mit „alle Spuren" — der
	// Auswahl-Dialog käme nie zum Zuge.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("runner.go: Start (StdinPipe): %w", err)
	}

	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		return fmt.Errorf("runner.go: Start (Start): %w", err)
	}

	r.cmd = cmd
	r.pid = cmd.Process.Pid
	r.stopping = false
	r.input = stdin
	r.mu.Unlock()

	r.emit("conv:state", RunState{Running: true})

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		r.readChannel(stdout, true)
	}()
	go func() {
		defer readers.Done()
		r.readChannel(stderr, false)
	}()

	go func() {
		// Erst wenn beide Kanäle zu Ende gelesen sind, darf gewartet werden:
		// Wait schließt die Leitungen, und was dann noch unterwegs ist, ginge
		// verloren — ausgerechnet die letzten Zeilen sind aber die
		// interessantesten.
		readers.Wait()
		waitErr := cmd.Wait()
		r.finish(waitErr)
	}()
	return nil
}

// finish räumt auf und meldet das Ende genau einmal.
func (r *Runner) finish(waitErr error) {
	r.mu.Lock()
	wasStopping := r.stopping
	r.cmd = nil
	r.pid = 0
	r.stopping = false
	// cmd.Wait hat die Eingabeleitung bereits geschlossen; hier wird nur noch
	// vergessen, dass es sie gab. Eine Antwort nach Laufende landet dann im
	// klaren Fehler statt in einem geschlossenen Rohr.
	r.input = nil
	r.mu.Unlock()

	state := RunState{Running: false, Stopping: wasStopping}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		// Sauber durchgelaufen.
	case errors.As(waitErr, &exitErr):
		state.ExitCode = exitErr.ExitCode()
	default:
		state.Error = waitErr.Error()
	}
	r.emit("conv:state", state)
}

// readChannel liest einen der beiden Ausgabekanäle.
//
// mayCarryEvents ist nur für den Hauptkanal wahr. Was sich dort nicht als
// Ereignis lesen lässt, wird nicht verworfen, sondern wie Bildschirmausgabe
// behandelt — genau dieser Fall tritt bei einer älteren Programmdatei ohne
// Datenkanal für JEDE Zeile ein.
func (r *Runner) readChannel(pipe io.Reader, mayCarryEvents bool) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLine)
	scanner.Split(splitLinesAndReturns)

	for scanner.Scan() {
		raw := scanner.Text()
		if mayCarryEvents {
			if event, ok := parseEvent(raw); ok {
				r.emit("conv:event", event)
				continue
			}
		}
		r.emitLogLine(raw)
	}
	if err := scanner.Err(); err != nil {
		r.emit("conv:log", LogLine{Text: "[gui] output channel: " + err.Error()})
	}
}

// parseEvent versucht, aus einer Zeile ein Ereignis zu machen.
//
// ab-av1 (Rust/clap) gibt als strukturierte Ereignisse JSON-Objekte mit einem
// "type"-Feld aus — z.B. das Fortschrittsobjekt von indicatif. Frühere
// NVENCForge-Ereignisse trugen "ev". Beide Formen werden erkannt; der Typ
// wird in beiden Fällen unter "ev" weitergereicht, damit die Abnehmer
// (savings.go, bookAndForward) unverändert bleiben.
func parseEvent(raw string) (map[string]any, bool) {
	line := strings.TrimSpace(raw)
	if !strings.HasPrefix(line, "{") {
		return nil, false
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return nil, false
	}
	if kind, ok := event["ev"].(string); ok && kind != "" {
		return event, true
	}
	// ab-av1: {"type":"Progress", ...} oder {"type":"Finished", ...}
	if kind, ok := event["type"].(string); ok && kind != "" {
		event["ev"] = strings.ToLower(kind)
		return event, true
	}
	return nil, false
}

// emitLogLine bereitet eine Zeile Bildschirmausgabe auf und schickt sie weiter.
func (r *Runner) emitLogLine(raw string) {
	if line, ok := toLogLine(raw); ok {
		r.emit("conv:log", line)
	}
}

// toLogLine macht aus roher Terminal-Ausgabe eine anzeigbare Zeile.
//
// Die Farben bleiben absichtlich erhalten: Das Fenster stellt sie genauso dar
// wie das Terminal, und ohne sie wäre der Unterschied zwischen einem Hinweis
// und einem Fehler nur noch am Wortlaut zu erkennen.
func toLogLine(raw string) (LogLine, bool) {
	back := 0
	for _, match := range cursorUpPattern.FindAllStringSubmatch(raw, -1) {
		count := 1
		if match[1] != "" {
			if parsed, err := strconv.Atoi(match[1]); err == nil {
				count = parsed
			}
		}
		back += count
	}

	text := strings.TrimRight(nonColorEscape.ReplaceAllString(raw, ""), " \t")
	visible := strings.TrimSpace(colorEscape.ReplaceAllString(text, ""))
	if visible == "" {
		if back == 0 {
			return LogLine{}, false
		}
		// Nichts zu zeigen, aber vorherige Zeilen sollen weg.
		return LogLine{Back: back}, true
	}
	return LogLine{Text: text, Back: back}, true
}

// splitLinesAndReturns trennt zusätzlich am Wagenrücklauf.
//
// Die Fortschrittsanzeige beendet ihre Zeile mit "\r" statt mit einem
// Zeilenumbruch. Ohne diese Trennung wüchse daraus eine einzige riesige Zeile,
// die erst am Ende des Laufs im Protokoll auftauchte.
func splitLinesAndReturns(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for index, character := range data {
		if character == '\n' || character == '\r' {
			return index + 1, data[:index], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// RequestStop löst den sauberen Abbruch aus — denselben, den Strg+C im
// Terminal auslöst. Ein zweiter Aufruf beendet den Konverter sofort; das ist
// sein eingebautes Verhalten und wird hier bewusst nicht nachgebaut.
func (r *Runner) RequestStop() error {
	r.mu.Lock()
	pid := r.pid
	if r.cmd == nil || pid == 0 {
		r.mu.Unlock()
		return errors.New("nothing is running")
	}
	alreadyStopping := r.stopping
	r.stopping = true
	input := r.input
	r.input = nil
	r.mu.Unlock()

	// Ein offener Auswahl-Dialog würde den Konverter im Lesen festhalten: Das
	// Abbruch-Signal erreicht ihn zwar, aber die Stelle, die auf die Antwort
	// wartet, sieht es nicht. Die Eingabe zu schließen löst sie sofort — dort
	// gilt dann die sichere Vorgabe (alle Spuren), und der Abbruch greift im
	// nächsten Schritt.
	if input != nil {
		_ = input.Close()
	}

	if err := requestCleanStop(pid); err != nil {
		return err
	}
	note := "[gui] Clean stop requested. The part already encoded is finalised as a playable preview."
	if alreadyStopping {
		note = "[gui] Stop requested a second time — NVENCForge quits immediately."
	}
	r.emit("conv:log", LogLine{Text: note})
	r.emit("conv:state", RunState{Running: true, Stopping: true})
	return nil
}

// Answer beantwortet eine Rückfrage des Konverters — heute die Spurauswahl.
//
// Erwartet wird genau die Zeile, die auch ein Mensch tippen würde: "1,3" für
// einzelne Nummern, leer für alle Spuren. Deshalb wird hier nichts umgeformt;
// nur der Zeilenumbruch kommt dazu, ohne den die Gegenseite weiterwartet.
//
// WICHTIG (am 2026-08-18 gemessen): Es darf immer nur EINE Antwort unterwegs
// sein, und erst nachdem die Frage angekündigt wurde. Jede Frage-Stelle des
// Konverters liest mit einem eigenen Puffer — zwei Antworten hintereinander
// landen beide im ersten davon, und die zweite Frage bekommt ihre nie zu
// sehen. Sie läuft dann still in „alle Spuren" statt in die getroffene Wahl.
func (r *Runner) Answer(text string) error {
	r.mu.Lock()
	input := r.input
	r.mu.Unlock()
	if input == nil {
		return errors.New("nothing is waiting for an answer")
	}

	// Ein Zeilenumbruch mitten in der Antwort würde aus einer Antwort zwei
	// machen — die zweite wäre genau die Antwort auf Vorrat, die oben
	// beschrieben ist.
	line := strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	if _, err := io.WriteString(input, line+"\n"); err != nil {
		return fmt.Errorf("runner.go: Answer: %w", err)
	}
	return nil
}
