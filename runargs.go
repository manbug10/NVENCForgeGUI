// NVENCForgeGUI (https://github.com/burnersen/NVENCForgeGUI)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

// runargs.go — aus den Schaltern der Oberfläche wird die Befehlszeile von ab-av1.
//
// Eine eigene Datei, weil das die Stelle ist, an der die Oberfläche und der
// Konverter sich berühren. ab-av1 (https://github.com/alexheretic/ab-av1)
// arbeitet mit UNTERBEFEHLEN statt mit Einzel-Schaltern:
//
//   - step    Qualitäts-/Krönungssuche (probe + encode bis zum VMAF-Ziel) und
//             Konvertierung — der Standardweg dieses Fensters.
//   - convert Konvertierung mit festem Encoder, ohne Suche.
//   - sample  Prüflauf: schreibt nur eine kurze Vorschau, wandelt nichts endgültig um.
//   - select  Stapelauswahl; hier nur für die Ordnerbeobachtung benutzt.
//   - vmaf    reine Messung (für spätere Erweiterungen vorgesehen).
//
// Für alle Optionen gilt dieselbe Regel wie früher: Steht ein Wert nicht im
// Fenster, wird nichts übergeben — dann entscheidet ab-av1 nach seiner
// Konfigurationsdatei (ab-av1.toml), und das ist die Vorgabe, die der Nutzer
// erwartet.
package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Die Unterbefehle, die dieses Fenster verwendet (Schlüssel in modeFlags).
const (
	commandStep    = "step"
	commandConvert = "convert"
	commandSample  = "sample"
	commandSelect  = "select"
)

// Erlaubte Werte der Auswahlfelder. Als Konstanten, damit ein Tippfehler beim
// Vergleich auffällt statt still den Standardfall zu wählen.
const (
	codecAV1  = "av1"
	codecH265 = "svt" // ab-av1: SVT-Hevc ist der H.265-Pfad (--svt / --hevc)

	encoderCPU    = "cpu"
	encoderNvidia = "nvidia"

	containerMP4 = "mp4"
	containerMKV = "mkv"

	resolutionOriginal = "original"

	audioCopy = "copy"
	audioAAC  = "aac"

	bitDepth8  = "8"
	bitDepth10 = "10"

	// Die Qualitätswahl. ab-av1 kennt kein "Aus" für die Suche: step sucht
	// immer. "off" lässt das Fenster deshalb auf den festen Unterbefehl
	// "convert" umschalten — Konvertieren ohne Krönungssuche.
	qualityAuto  = "auto"
	qualityOff   = "off"
	qualityFixed = "fixed"

	// qualityCheck ist der Prüflauf: "sample" schreibt nur eine kurze Vorschau
	// und konvertiert nichts endgültig.
	qualityCheck = "check"

	// Auto-Crop: "on" schneidet schwarze Balken weg ("off" lässt es; der leere
	// Wert heißt wie bisher "das Fenster hat nichts dazu gesagt").
	cropOn  = "on"
	cropOff = "off"
)

// Grenzen der Crf-Skala. SVT-Hevc (H.265) reicht bis 63, AV1 ebenfalls;
// untere Grenze ist 1.
const (
	minCQ     = 1
	maxCQH265 = 63
	maxCQAV1  = 63
)

// targetVmafFloor und -Ceiling begrenzen das VMAF-Ziel, wie es das Fenster
// vergibt. ab-av1 selbst prüft nur 0..100; der nutzbare Bereich liegt
// deutlich höher — unterhalb von ~85 lohnt das Krönen kaum noch.
const (
	targetVmafFloor  = 85.0
	targetVmafCeiling = 99.0
)

// clampTargetVmaf begrenzt einen Zielwert auf den sinnvollen Bereich.
func clampTargetVmaf(vmaf float64) float64 {
	switch {
	case vmaf < targetVmafFloor:
		return targetVmafFloor
	case vmaf > targetVmafCeiling:
		return targetVmafCeiling
	default:
		return vmaf
	}
}

// formatTargetVmaf schreibt den Wert ohne überflüssige Nullen ("93", nicht
// "93.000000"), damit die Befehlszeile lesbar bleibt.
func formatTargetVmaf(vmaf float64) string {
	return strconv.FormatFloat(vmaf, 'f', -1, 64)
}

// Die beiden Wege, aus Einzeldateien wieder eine Datei zu machen. Sie brauchen
// dieselbe Zusammenstellung, liefern aber Verschiedenes:
//
//   - modeJoin: alles 1:1 kopiert (ab-av1 hat keinen Join-Befehl; dieser Weg
//     bleibt über den mitgelieferten FFmpeg-Pfad erhalten).
//   - modeJoinDavinci: Ton wird nach AAC umkodiert, wo DaVinci ihn sonst nicht
//     liest, Untertitel werden gereinigt.
//
// Sie stehen als Konstanten da, weil sie als Einzige eine eigene Prüfung haben
// — ein Tippfehler im Vergleich würde sie still überspringen.
const (
	modeJoin        = "join"
	modeJoinDavinci = "joindavinci"
)

// needsJoinOrder sagt, ob ein Modus die Zusammenstellung "genau ein Bild zuerst"
// braucht. Beide Zusammenfüge-Wege reichen dem Konverter dieselbe Liste.
func needsJoinOrder(mode string) bool {
	return mode == modeJoin || mode == modeJoinDavinci
}

// RunRequest ist das, was die Oberfläche für einen Lauf schickt.
type RunRequest struct {
	// Area sagt, aus welchem Bereich des Fensters der Auftrag kommt:
	// leer/"convert" für das Umwandeln von Hand, "split" fürs Zerlegen,
	// "join" fürs Zusammenfügen, "watch" für den beobachteten Ordner. Davon
	// hängt ab, auf welchem Platz er läuft und in welcher Anzeige seine
	// Meldungen landen.
	Area       string   `json:"area"`
	Mode       string   `json:"mode"`     // "" = konvertieren, sonst Schlüssel aus modeFlags
	Parallel   int      `json:"parallel"` // gleichzeitige Läufe (1–3), 0 = einer
	Files      []string `json:"files"`
	Codec      string   `json:"codec"`      // "", "av1" oder "svt"
	Encoder    string   `json:"encoder"`    // "", "cpu" oder "nvidia"
	Container  string   `json:"container"`  // "", "mp4" oder "mkv"
	Resolution string   `json:"resolution"` // "", "original"
	Audio      string   `json:"audio"`      // "", "copy" oder "aac"
	BitDepth   string   `json:"bitDepth"`   // "", "8" oder "10"
	Quality    string   `json:"quality"`    // "", "auto", "off" oder "fixed"
	Crop       string   `json:"crop"`       // "", "on" oder "off"
	FixedCQ    int      `json:"fixedCQ"`
	KeepSource bool     `json:"keepSource"`

	// MeasuredCQ enthält die Crf-Werte, die ein vorangegangener Prüflauf für
	// einzelne Dateien schon ermittelt hat (Pfad → Crf). Benutzt wird ein Wert
	// nur, wenn die Qualitätswahl weiterhin auf "auto" steht — bei einem
	// festen Wert hat der Nutzer selbst entschieden.
	MeasuredCQ map[string]int `json:"measuredCQ,omitempty"`

	// Shutdown ist der WUNSCH "PC ausschalten, wenn alles fertig ist" — kein
	// Schalter für die Befehlszeile. ab-av1 kennt ohnehin keinen eigenen
	// Abschalt-Schalter; ausgeführt wird der Wunsch von shutdown.go, wenn
	// wirklich alles leer ist.
	Shutdown bool `json:"shutdown"`

	// SuppressConverterShutdown ist KEINE Wahl aus dem Fenster, sondern ein
	// Schutz — bleibt der Form halber stehen. ab-av1 schaltet den Rechner nie
	// selbst ab, also ist hier nichts zu unterdrücken.
	SuppressConverterShutdown bool `json:"-"`

	// CounterFlags ist die Auskunft, ob die vorhandene Programmdatei alle
	// Long-Flags kennt, die das Fenster in beide Richtungen setzen kann.
	// Bei ab-av1 sind die Flags stabil; app.go setzt das Feld aus dem Status.
	CounterFlags bool `json:"-"`
}

// buildJobs macht aus einer Anfrage die einzelnen Aufträge für den Verteiler.
//
// Beim KONVERTIEREN bekommt jede Datei ihren eigenen ab-av1-Prozess. Nur so
// können mehrere gleichzeitig laufen, ohne dass zwei Konverter sich um
// dieselbe Datei streiten — die Begründung samt Messung steht im Kopf von
// dispatcher.go. (ab-av1 selbst würde einen Stapel in einem Durchlauf
// verarbeiten; das Fenster bleibt aber je Datei, damit Fortschritt, Stopp und
// Sparbuch pro Datei funktionieren.)
//
// ZUSAMMENFÜGEN bekommt einen Auftrag je Bild-Grundlage. Der Konverter baut
// pro Lauf genau eine Datei, ein Stapel zerlegter Filme sind aber viele — und
// die Zuordnung steht im Namen (joinfiles.go). Sie laufen nacheinander: Der
// Bereich hat genau einen Platz, mehr wäre auch nichts wert, weil hier die
// Festplatte bremst und nicht die Grafikkarte.
func buildJobs(request RunRequest) ([]job, error) {
	if needsJoinOrder(request.Mode) {
		return buildJoinJobs(request)
	}
	if request.Mode != "" {
		args, err := buildConverterArgs(request)
		if err != nil {
			return nil, err
		}
		return []job{{label: request.Mode, args: args}}, nil
	}

	if len(request.Files) == 0 {
		return nil, fmt.Errorf("runargs.go: buildJobs: the queue is empty")
	}

	jobs := make([]job, 0, len(request.Files))
	for _, file := range request.Files {
		single := request
		single.Files = []string{file}
		// Ein schon gemessener Crf macht die Suche für DIESE Datei überflüssig.
		// Er wird als fester Wert übergeben — genau das ist gewollt, denn er IST
		// das Ergebnis der Suche, nur eben von vorhin.
		if cq, ok := measuredCQFor(request, file); ok {
			single.Quality = qualityFixed
			single.FixedCQ = cq
		}
		args, err := buildConverterArgs(single)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job{label: filepath.Base(file), key: file, args: args})
	}
	return jobs, nil
}

// measuredCQFor sucht den für diese Datei bereits gemessenen CQ heraus.
//
// Zwei Bedingungen, beide nötig.
//
// Erstens die Qualitätswahl: "auto" heißt ausdrücklich messen, der LEERE Wert
// heißt "die INI entscheidet" — und da ein Prüflauf überhaupt einen CQ
// geliefert hat, stand sie auf Auto-CQ. Beides ist also in Ordnung. "fixed"
// und "off" sind dagegen Entscheidungen des Nutzers; eine Messung von vorhin
// darf sie nicht überstimmen.
//
// Zweitens muss der Wert in der Skala liegen — ein kaputter Wert aus der
// Oberfläche darf nicht ungeprüft zum Konverter durchgereicht werden.
//
// Pfade werden ohne Rücksicht auf Groß- und Kleinschreibung verglichen: unter
// Windows ist "C:\Filme\A.mkv" dieselbe Datei wie "c:\filme\a.mkv".
func measuredCQFor(request RunRequest, file string) (int, bool) {
	if request.Quality != qualityAuto && request.Quality != "" {
		return 0, false
	}
	if len(request.MeasuredCQ) == 0 {
		return 0, false
	}
	upperBound := maxCQH265
	if request.Codec == codecAV1 {
		upperBound = maxCQAV1
	}
	wanted := strings.ToLower(file)
	for path, cq := range request.MeasuredCQ {
		if strings.ToLower(path) != wanted {
			continue
		}
		if cq < minCQ || cq > upperBound {
			return 0, false
		}
		return cq, true
	}
	return 0, false
}

// buildJoinJobs macht aus einer Join-Ablage einen Auftrag je Bild-Grundlage.
//
// Die Aufteilung selbst steht in joinfiles.go — dort, wo auch entschieden wird,
// welche Datei was ist. ab-av1 kennt keinen join-Befehl; die Zusammenfügung
// bleibt ein Weg über den mitgelieferten FFmpeg-Pfad (joinArgOrder sortiert
// Bild → Ton → Untertitel, wie ihn der Verteiler am Modus erkennt).
//
// Beschriftet wird der Auftrag mit seiner Bild-Grundlage: In der Anzeige steht
// dann der Filmname und nicht fünfmal "join".
func buildJoinJobs(request RunRequest) ([]job, error) {
	groups, err := joinGroupPaths(request.Files)
	if err != nil {
		return nil, err
	}

	jobs := make([]job, 0, len(groups))
	for _, group := range groups {
		ordered, err := joinArgOrder(group)
		if err != nil {
			return nil, err
		}
		// Das erste Argument ist der Modus ("join"/"joindavinci"); er sagt dem
		// Verteiler, dass dieser Lauf über FFmpeg läuft und nicht über ab-av1.
		args := append([]string{request.Mode}, ordered...)
		// Die Bild-Grundlage steht nach joinArgOrder vorn — sie ist die
		// Beschriftung des Auftrags.
		jobs = append(jobs, job{label: filepath.Base(ordered[0]), args: args})
	}
	return jobs, nil
}

// joinLabelOf nennt die Bild-Grundlage einer Gruppe. Findet sich keine (was
// joinGroupPaths ausschließt), steht die erste Datei da — eine leere
// Beschriftung wäre in der Warteschlange schlimmer als eine ungenaue.
func joinLabelOf(group []string) string {
	for _, file := range classifyJoinFiles(group) {
		if file.Kind == joinKindVideo {
			return file.Path
		}
	}
	return group[0]
}

// buildConverterArgs setzt die ab-av1-Befehlszeile zusammen.
//
// Das ERSTE Argument ist immer der Unterbefehl (step/convert/sample):
// ab-av1 erkennt seinen Betriebsmodus an genau dieser Stelle.
//
// Die Werkzeug-Modi (join/joindavinci/split) sind reine FFmpeg-Arbeiten und
// laufen über joinfiles.go bzw. den ffmpeg-Pfad — sie erreichen diese Funktion
// nicht; würden sie hier landen, ist das ein Fehler des Aufrufers.
func buildConverterArgs(request RunRequest) ([]string, error) {
	if len(request.Files) == 0 {
		return nil, fmt.Errorf("runargs.go: buildConverterArgs: the queue is empty")
	}
	if request.Mode != "" {
		return nil, fmt.Errorf("runargs.go: buildConverterArgs: tool mode %q does not run through ab-av1", request.Mode)
	}

	// Unterbefehl nach Qualitätswahl:
	//   "check"        → sample  (nur Vorschau mit Krönungssuche, keine Umwandlung)
	//   "off"          → convert (Umwandlung ohne Suche)
	//   sonst (auto/""/fixed)   → step    (Suche + Umwandlung)
	var command string
	switch request.Quality {
	case qualityCheck:
		command = commandSample
	case qualityOff:
		command = commandConvert
	default:
		command = commandStep
	}
	args := []string{command}

	// Codec / Encoder. ab-av1 kodiert standardmäßig AV1 (SVT-AV1); --hevc
	// schaltet auf H.265 um, dafür sorgt --svt für den SVT-Hevc-Encoder statt
	// x264/x265. Ein GPU-Encoder wird ausdrücklich gesetzt (--nvenc-hevc für
	// H.265; AV1 auf der Karte wäre --svt-av1). Steht nichts dazu da, gilt
	// die Vorgabe aus ab-av1.toml.
	isHevc := request.Codec == codecH265
	switch request.Encoder {
	case encoderNvidia:
		if isHevc {
			args = append(args, "--nvenc-hevc")
		} else {
			args = append(args, "--svt-av1")
		}
	case encoderCPU:
		// CPU: bei H.265 SVT-Hevc erzwingen, bei AV1 ist SVT-AV1 schon Vorgabe.
		if isHevc {
			args = append(args, "--svt")
		}
	default:
		if isHevc {
			args = append(args, "--hevc")
		}
	}

	// Container: ab-av1 schreibt MKV; --mp4 erzwingt die MP4-Hülle.
	// mkv ist die Vorgabe — bewusst kein Flag.
	if request.Container == containerMP4 {
		args = append(args, "--mp4")
	}

	// Auflösung: ab-av1 skaliert standardmäßig auf 1080p herunter;
	// --no-downscale hält die Originalhöhe. Der Gegenfall (herunterskalieren)
	// ist die Vorgabe und braucht kein Flag.
	if request.Resolution == resolutionOriginal {
		args = append(args, "--no-downscale")
	}

	// Ton: "copy" = alle Tonspuren unverändert übernehmen (--copy-audio),
	// "aac" = auf AAC umkodieren (--audio-codec aac). Der leere Wert heißt
	// wie bisher: ab-av1.toml entscheidet.
	switch request.Audio {
	case audioCopy:
		args = append(args, "--copy-audio")
	case audioAAC:
		args = append(args, "--audio-codec", "aac")
	}

	// Bittiefe: --tenbit erzwingt 10 Bit; --eight-bit ist die Vorgabe von
	// ab-av1 und wird nur ausdrücklich gesetzt, wenn die TOML anders steht
	// und das Fenster in beide Richtungen durchgreifen kann (CounterFlags).
	switch request.BitDepth {
	case bitDepth10:
		args = append(args, "--tenbit")
	case bitDepth8:
		if request.CounterFlags {
			args = append(args, "--eight-bit")
		}
	}

	// Auto-Crop: ab-av1 hat keinen eigenen Crop-Schalter; schwarze Ränder
	// entfernt es über einen Filter vor dem Encoder — dieselbe Kette würde
	// ein fester --filter-video crop=... sein, doch die Randmaße kennt nur
	// ein Vorlauf. Das Fenster bietet den Schalter deshalb nur an, wenn die
	// exe ihn nachweislich versteht; bis dahin bleibt er ohne Wirkung und
	// ohne Flag (leerer Wert = ab-av1.toml entscheidet).
	if request.Crop == cropOn && request.CounterFlags {
		args = append(args, "--filter-video", "cropdetect")
	}

	// Qualität: fester Crf-Wert oder VMAF-Ziel.
	qualityArgs, err := buildQualityArgs(request)
	if err != nil {
		return nil, err
	}
	args = append(args, qualityArgs...)

	// Original behalten: ab-av1 ersetzt die Quelle nach gelungener
	// Konvertierung; --keep-original lässt sie liegen. Kein Flag heißt:
	// die TOML entscheidet — dort steht keep_original als Vorgabe.
	if request.KeepSource {
		args = append(args, "--keep-original")
	}

	// Hier fehlen mit Absicht "-shutdown" und "-noshutdown": ab-av1 fährt den
	// Rechner nie selbst herunter — dafür ist ausschließlich shutdown.go
	// zuständig, wenn wirklich alles leer ist.

	return append(args, request.Files...), nil
}

// buildQualityArgs entscheidet über die Qualitätswahl.
//
// Getrennt, weil hier als Einzigem etwas geprüft werden muss: Ein Crf außerhalb
// der Skala würde ab-av1 zwar abfangen, aber erst nachdem der Lauf gestartet
// ist — die Meldung gehört vorher ins Fenster.
//
// Die Zuordnung der alten CQ-Welt zu ab-av1:
//   - "auto"      → step sucht das günstigste Crf von selbst; das Fenster setzt
//     nur die Grenzen (--min-crf/--max-crf) und das VMAF-Ziel
//     (--target-quality), wenn der Nutzer eines angegeben hat.
//   - "off"       → kein Qualitätsflag; convert nimmt --crf aus der TOML bzw.
//     dessen Vorgabe.
//   - "fixed"     → --crf <Wert>, unabhängig von der Suche.
//   - "check"     → sample; die Suche läuft, umgewandelt wird nichts.
func buildQualityArgs(request RunRequest) ([]string, error) {
	switch request.Quality {
	case qualityAuto:
		// Die Suche selbst braucht keine Flags — step krönt von Haus aus und
		// hält sich dabei an target_quality bzw. min/max-crf aus ab-av1.toml.
		return nil, nil
	case qualityOff:
		// Kein Such-Flag; der convert-Unterbefehl nimmt den Crf aus der TOML.
		return nil, nil
	case qualityCheck:
		// sample sucht selbst; kein zusätzliches Qualitätsflag.
		return nil, nil
	case qualityFixed:
		upperBound := maxCQH265
		if request.Codec == codecAV1 || request.Codec == "" {
			upperBound = maxCQAV1
		}
		if request.FixedCQ < minCQ || request.FixedCQ > upperBound {
			return nil, fmt.Errorf(
				"runargs.go: buildQualityArgs: CRF must be between %d and %d for this codec, got %d",
				minCQ, upperBound, request.FixedCQ)
		}
		return []string{"--crf", strconv.Itoa(request.FixedCQ)}, nil
	default:
		// Leer oder unbekannt: nichts übergeben, es gilt ab-av1.toml.
		return nil, nil
	}
}
