package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"
	"strings"
	"time"

	_ "embed"

	"flag"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgb/xtest"
	"github.com/getlantern/systray"
)

// Audio configuration constants
const (
	sampleRate      = 16000
	channels        = 1
	recordTimeout   = 1 * time.Second        // Reduced from 2s to 1s for faster chunks
	silenceDuration = 300 * time.Millisecond // Reduced from 500ms to 300ms for quicker detection
	energyThreshold = 80                     // Energy threshold for silence detection
)

// AudioChunk represents a block of recorded samples along with the time it was captured.
type AudioChunk struct {
	timestamp time.Time
	data      []int16
}

// Create reusable buffers at package level
var (
	wavBuffer  bytes.Buffer
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}
	//go:embed icon_off.png
	iconOff []byte
	//go:embed icon_on.png
	iconOn []byte
)

// AudioChunkParams defines the parameters for chunking audio
const (
	// Adjust these values based on your needs
	minChunkDuration = 1 * time.Second        // Minimum duration for meaningful transcription
	chunkOverlap     = 500 * time.Millisecond // Overlap between chunks to avoid cutting words
)

// Configuration constants
var (
	serverHost = flag.String("host", "localhost", "Whisper server host")
	serverPort = flag.Int("port", 36124, "Whisper server port")
)

type KeyboardSimulator struct {
	conn   *xgb.Conn
	keymap map[rune]byte
}

func newKeyboardSimulator() (*KeyboardSimulator, error) {
	X, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("connecting to X server: %w", err)
	}

	if err := xtest.Init(X); err != nil {
		X.Close()
		return nil, fmt.Errorf("initializing XTEST: %w", err)
	}

	// Get keyboard mapping
	keyboard := &KeyboardSimulator{conn: X}
	if err := keyboard.initKeymap(); err != nil {
		X.Close()
		return nil, fmt.Errorf("initializing keymap: %w", err)
	}

	return keyboard, nil
}

func (k *KeyboardSimulator) initKeymap() error {
	// Query the server for the first keycode
	setup := xproto.Setup(k.conn)
	mapping, err := xproto.GetKeyboardMapping(k.conn,
		setup.MinKeycode,
		byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err != nil {
		return fmt.Errorf("getting keyboard mapping: %w", err)
	}

	// Create keymap
	k.keymap = make(map[rune]byte)
	keysPerCode := int(mapping.KeysymsPerKeycode)

	// Iterate through keycodes
	for keycode := int(setup.MinKeycode); keycode <= int(setup.MaxKeycode); keycode++ {
		for offset := 0; offset < keysPerCode; offset++ {
			// Calculate index in the keysyms array
			idx := (keycode-int(setup.MinKeycode))*keysPerCode + offset
			if idx >= len(mapping.Keysyms) {
				continue
			}

			keysym := mapping.Keysyms[idx]
			if keysym == 0 {
				continue
			}

			// Convert keysym to rune if it represents a character
			if r := keysymToRune(keysym); r != 0 {
				k.keymap[r] = byte(keycode)
			}
		}
	}

	return nil
}

func keysymToRune(keysym xproto.Keysym) rune {
	// Common punctuation marks
	punctuation := map[xproto.Keysym]rune{
		0x003f: '?',  // Question mark
		0x002e: '.',  // Period
		0x002c: ',',  // Comma
		0x0021: '!',  // Exclamation mark
		0x0027: '\'', // Single quote
		0x0022: '"',  // Double quote
		0x0028: '(',  // Left parenthesis
		0x0029: ')',  // Right parenthesis
		0x002d: '-',  // Hyphen
		0x005f: '_',  // Underscore
	}

	// Check punctuation map first
	if r, ok := punctuation[keysym]; ok {
		return r
	}

	// Basic ASCII conversion
	if keysym < 0x100 {
		return rune(keysym)
	}

	// Unicode direct mapping
	if keysym >= 0x1000000 {
		return rune(keysym - 0x1000000)
	}

	// Common Latin-1 characters
	if keysym >= 0x20 && keysym <= 0x7e {
		return rune(keysym)
	}

	return 0
}

func (k *KeyboardSimulator) typeText(text string) {
	// Type the transcribed text
	for _, char := range text {
		keycode, ok := k.keymap[char]
		if !ok {
			log.Printf("Skipping unknown character: %c (keycode not found)", char)
			continue
		}

		// Handle shifted characters (including ?)
		needsShift := char >= 'A' && char <= 'Z' ||
			strings.ContainsRune("?!@#$%^&*()_+{}|:\"<>~", char)

		if needsShift {
			xtest.FakeInput(k.conn, 2, 50, 0, 0, 0, 0, 0) // Press Shift
		}

		// Press and release the key
		xtest.FakeInput(k.conn, 2, keycode, 0, 0, 0, 0, 0)
		time.Sleep(5 * time.Millisecond)
		xtest.FakeInput(k.conn, 3, keycode, 0, 0, 0, 0, 0)
		time.Sleep(5 * time.Millisecond)

		if needsShift {
			xtest.FakeInput(k.conn, 3, 50, 0, 0, 0, 0, 0) // Release Shift
		}
	}

	// Always append a space after each chunk
	if spaceCode, ok := k.keymap[' ']; ok {
		xtest.FakeInput(k.conn, 2, spaceCode, 0, 0, 0, 0, 0)
		time.Sleep(5 * time.Millisecond)
		xtest.FakeInput(k.conn, 3, spaceCode, 0, 0, 0, 0, 0)
		time.Sleep(5 * time.Millisecond)
	}
}

func main() {
	flag.Parse()
	systray.Run(onReady, onExit)
}

func onReady() {
	// Try setting a default icon first
	systray.SetIcon(iconOff)
	systray.SetTitle("WhisperType")
	systray.SetTooltip("Speech-to-text (inactive)")

	mQuit := systray.AddMenuItem("Quit", "Quit WhisperType")

	keyboard, err := newKeyboardSimulator()
	if err != nil {
		log.Fatal(err)
	}

	// Setup key monitoring for all possible modifier combinations
	root := xproto.Setup(keyboard.conn).DefaultScreen(keyboard.conn).Root
	modifiers := []uint16{
		xproto.ModMask4 | xproto.ModMaskShift,                                        // Super(Command)+Shift
		xproto.ModMask4 | xproto.ModMaskShift | xproto.ModMaskLock,                   // With CapsLock
		xproto.ModMask4 | xproto.ModMaskShift | xproto.ModMask2,                      // With NumLock
		xproto.ModMask4 | xproto.ModMaskShift | xproto.ModMaskLock | xproto.ModMask2, // Both
	}

	for _, mod := range modifiers {
		err = xproto.GrabKeyChecked(
			keyboard.conn,
			false,
			root,
			mod,
			38, // 'a' keycode
			xproto.GrabModeAsync,
			xproto.GrabModeAsync,
		).Check()
		if err != nil {
			log.Printf("Warning: Failed to grab key with modifier %d: %v", mod, err)
		}
	}

	// Handle quit from menu
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	var (
		isActive bool
		cancel   context.CancelFunc
	)

	// Handle key events
	for {
		ev, err := keyboard.conn.WaitForEvent()
		if err != nil {
			continue
		}

		switch event := ev.(type) {
		case xproto.KeyPressEvent:
			if event.Detail == 38 { // 'a' keycode
				if !isActive {
					// Start recording
					systray.SetTemplateIcon(iconOn, iconOn)
					systray.SetTooltip("Speech-to-text (active)")

					ctx, cancelFn := context.WithCancel(context.Background())
					cancel = cancelFn
					go func() {
						if err := run(ctx, keyboard); err != nil {
							log.Printf("Error: %v", err)
						}
					}()
				} else {
					// Stop recording
					if cancel != nil {
						cancel()
					}
					systray.SetIcon(iconOff)
					systray.SetTooltip("Speech-to-text (inactive)")
				}
				isActive = !isActive
			}
		}
	}
}

func onExit() {
	// Cleanup code here
}

func run(ctx context.Context, keyboard *KeyboardSimulator) error {
	audioChan := make(chan AudioChunk, 10)
	go recordLoop(ctx, recordTimeout, audioChan)

	var (
		phraseBuffer    []int16
		transcriptLines []string
		silenceStart    time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return finalizeTranscript(phraseBuffer, transcriptLines)
		default:
		}

		chunk, ok := readNextChunk(audioChan)
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		if isSilent(chunk.data, energyThreshold) {
			if silenceStart.IsZero() {
				silenceStart = chunk.timestamp
				log.Printf("Silence started at %v", silenceStart)
			}

			if len(phraseBuffer) > 0 && time.Since(silenceStart) > silenceDuration {
				text, err := transcribeChunk(phraseBuffer)
				if err != nil {
					return fmt.Errorf("transcription error: %w", err)
				}
				transcriptLines = append(transcriptLines, text)
				log.Printf("Typing: %s", text)
				keyboard.typeText(text)
				phraseBuffer = nil
				silenceStart = time.Time{}
			}
			continue
		}

		// Speech detected
		if !silenceStart.IsZero() {
			log.Printf("Speech detected after %v of silence", chunk.timestamp.Sub(silenceStart))
		}
		silenceStart = time.Time{}
		phraseBuffer = append(phraseBuffer, chunk.data...)
	}
}

func readNextChunk(audioChan chan AudioChunk) (AudioChunk, bool) {
	select {
	case chunk := <-audioChan:
		return chunk, true
	default:
		return AudioChunk{}, false
	}
}

func finalizeTranscript(buffer []int16, lines []string) error {
	if len(buffer) > 0 {
		text, err := transcribeChunk(buffer)
		if err != nil {
			return fmt.Errorf("final transcription error: %w", err)
		}
		lines = append(lines, text)
		fmt.Printf("\nFinal transcription: %s\n", text)
	}

	fmt.Println("\nComplete Transcript:")
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

// recordLoop runs the 'parec' command to obtain raw audio from PulseAudio.
// It reads fixed-size chunks corresponding to chunkDuration and sends them on audioChan.
func recordLoop(ctx context.Context, chunkDuration time.Duration, audioChan chan<- AudioChunk) {
	// Calculate the number of bytes (16-bit samples = 2 bytes).
	chunkBytes := int(float64(chunkDuration)/float64(time.Second)) * sampleRate * 2

	// Start the 'parec' command.
	cmd := exec.CommandContext(ctx, "parec", "--format=s16le", fmt.Sprintf("--rate=%d", sampleRate), fmt.Sprintf("--channels=%d", channels))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to get parec stdout: %v", err)
		close(audioChan)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start parec: %v", err)
		close(audioChan)
		return
	}

	buffer := make([]byte, chunkBytes)
	for {
		_, err := io.ReadFull(stdout, buffer)
		if err != nil {
			// Exit when context is canceled or an error occurs.
			break
		}

		// Copy buffer as it will be reused.
		chunkDataBytes := make([]byte, len(buffer))
		copy(chunkDataBytes, buffer)

		nSamples := len(chunkDataBytes) / 2
		samples := make([]int16, nSamples)
		for i := 0; i < nSamples; i++ {
			samples[i] = int16(binary.LittleEndian.Uint16(chunkDataBytes[i*2 : i*2+2]))
		}

		audioChan <- AudioChunk{
			timestamp: time.Now(),
			data:      samples,
		}
	}
	close(audioChan)
}

// isSilent computes the average absolute amplitude of the samples.
// It prints the average energy for debugging, then compares it to the threshold.
func isSilent(data []int16, threshold int) bool {
	var sum int64
	for _, sample := range data {
		if sample < 0 {
			sample = -sample
		}
		sum += int64(sample)
	}
	avg := sum / int64(len(data))
	// Debug: print the computed average. (Comment out the next line if too verbose.)
	log.Printf("Computed average energy: %d", avg)
	return avg < int64(threshold)
}

// transcribeChunk sends a smaller portion of audio for transcription
func transcribeChunk(samples []int16) (string, error) {
	// Reuse existing transcribe function but with smaller chunks
	wavBuffer.Reset()

	if err := writeWavToBuffer(&wavBuffer, samples, sampleRate, channels); err != nil {
		return "", fmt.Errorf("writing WAV buffer: %w", err)
	}

	var b bytes.Buffer
	writer := multipart.NewWriter(&b)

	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}

	if _, err := io.Copy(part, &wavBuffer); err != nil {
		return "", fmt.Errorf("copying buffer: %w", err)
	}

	if err := writer.WriteField("response_format", "json"); err != nil {
		return "", fmt.Errorf("adding response format field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("closing writer: %w", err)
	}

	serverURL := fmt.Sprintf("http://%s:%d/inference", *serverHost, *serverPort)
	req, err := http.NewRequest("POST", serverURL, &b)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("bad status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}

	// Clean up the text
	text := strings.TrimSpace(result.Text)
	if text == "[BLANK_AUDIO]" || text == "\n[BLANK_AUDIO]" {
		return "", nil
	}

	return text, nil
}

// transcribeInChunks processes the audio in smaller chunks with overlap
func transcribeInChunks(samples []int16) (string, error) {
	samplesPerChunk := int(minChunkDuration.Seconds() * float64(sampleRate))
	overlapSamples := int(chunkOverlap.Seconds() * float64(sampleRate))

	if len(samples) <= samplesPerChunk {
		return transcribeChunk(samples)
	}

	var fullText strings.Builder

	// Process chunks with overlap
	for start := 0; start < len(samples); start += samplesPerChunk - overlapSamples {
		end := start + samplesPerChunk
		if end > len(samples) {
			end = len(samples)
		}

		chunk := samples[start:end]
		text, err := transcribeChunk(chunk)
		if err != nil {
			return "", fmt.Errorf("transcribing chunk at %d: %w", start, err)
		}

		fullText.WriteString(text)
		fullText.WriteString(" ") // Add space between chunks
	}

	return fullText.String(), nil
}

// writeWavToBuffer writes WAV data directly to a buffer
func writeWavToBuffer(buffer *bytes.Buffer, samples []int16, sampleRate, channels int) error {
	const bitsPerSample = 16
	byteRate := uint32(sampleRate * channels * (bitsPerSample / 8))
	blockAlign := uint16(channels * (bitsPerSample / 8))

	var dataBuf bytes.Buffer
	for _, sample := range samples {
		if err := binary.Write(&dataBuf, binary.LittleEndian, sample); err != nil {
			return fmt.Errorf("writing sample: %w", err)
		}
	}
	dataSize := uint32(dataBuf.Len())

	// Write headers
	buffer.Write([]byte("RIFF"))
	binary.Write(buffer, binary.LittleEndian, uint32(36+dataSize))
	buffer.Write([]byte("WAVE"))

	// "fmt " subchunk.
	buffer.Write([]byte("fmt "))
	binary.Write(buffer, binary.LittleEndian, uint32(16)) // PCM subchunk size
	binary.Write(buffer, binary.LittleEndian, uint16(1))  // AudioFormat PCM = 1
	binary.Write(buffer, binary.LittleEndian, uint16(channels))
	binary.Write(buffer, binary.LittleEndian, uint32(sampleRate))
	binary.Write(buffer, binary.LittleEndian, uint32(byteRate))
	binary.Write(buffer, binary.LittleEndian, blockAlign)
	binary.Write(buffer, binary.LittleEndian, uint16(bitsPerSample))

	// "data" subchunk.
	buffer.Write([]byte("data"))
	binary.Write(buffer, binary.LittleEndian, uint32(dataSize))
	buffer.Write(dataBuf.Bytes())

	return nil
}

// clearScreen sends ANSI escape codes to clear the terminal.
func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

// printTranscript writes each transcription line to stdout.
func printTranscript(lines []string) {
	for _, line := range lines {
		fmt.Println(line)
	}
}
