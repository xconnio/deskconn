package deskconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phin1x/go-ipp"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/xconn-go"
)

type PrintMode string

const (
	PrintModeDisabled PrintMode = "disabled"
	PrintModeAccept   PrintMode = "accept"
	PrintModeHost     PrintMode = "host"
)

type PrinterInfo struct {
	Name     string `json:"name"`
	PPDModel string `json:"ppd"`
}

type PrintJobStatus struct {
	JobID     string `json:"job_id"`
	Printer   string `json:"printer"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type Printer struct {
	cups         *printerCUPSClient
	retryDelay   time.Duration
	printingMode func() (PrintMode, error)
}

func NewPrinter() *Printer {
	return &Printer{
		cups:       newPrinterCUPSClient("localhost"),
		retryDelay: 2 * time.Second,
	}
}

func (p *Printer) handleListPrinters(ctx context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	mode, err := p.currentPrintMode()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	if mode != PrintModeHost {
		return xconn.NewInvocationError(ErrOperationFailed,
			"printer hosting disabled; run `deskconn print enable --host-printers` to enable")
	}

	infos, err := p.cups.PrintersInfo(ctx)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	result := make([]map[string]any, len(infos))
	for i, info := range infos {
		result[i] = map[string]any{name: info.Name, "ppd": info.PPDModel}
	}

	return xconn.NewInvocationResult(result)
}

func (p *Printer) handlePrint() xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		mode, err := p.currentPrintMode()
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		if mode == PrintModeDisabled {
			return xconn.NewInvocationError(ErrOperationFailed, "printing disabled; run `deskconn print enable` to enable")
		}

		printer, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, "invalid or missing printer name")
		}
		filename, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, "invalid or missing file name")
		}
		data, err := inv.ArgBytes(2)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, "invalid or missing print data")
		}

		jobID := fmt.Sprintf("%s-%d", printer, time.Now().UnixNano())
		if strings.TrimSpace(printer) == "" {
			return xconn.NewInvocationError(ErrInvalidArgument, "empty printer name")
		}
		if len(data) == 0 {
			return xconn.NewInvocationError(ErrInvalidArgument, "empty print payload")
		}

		if err := p.ExecutePrint(ctx, printer, filename, data); err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult(jobID)
	}
}

func (p *Printer) currentPrintMode() (PrintMode, error) {
	if p.printingMode != nil {
		return p.printingMode()
	}
	return CurrentPrintMode()
}

func EnablePrinting() error {
	return SetPrintMode(PrintModeAccept)
}

func EnablePrinterHosting() error {
	return SetPrintMode(PrintModeHost)
}

func SetPrintMode(mode PrintMode) error {
	switch mode {
	case PrintModeAccept, PrintModeHost, PrintModeDisabled:
		return updatePrintingConfig(mode)
	default:
		return fmt.Errorf("unsupported print mode %q", mode)
	}
}

func DisablePrinting() error {
	return SetPrintMode(PrintModeDisabled)
}

func CurrentPrintMode() (PrintMode, error) {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		return PrintModeDisabled, err
	}
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if err != nil {
		if os.IsNotExist(err) {
			return PrintModeDisabled, nil
		}
		return PrintModeDisabled, err
	}
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return PrintModeDisabled, err
	}
	switch config.Printing.Mode {
	case PrintModeAccept, PrintModeHost:
		return config.Printing.Mode, nil
	case PrintModeDisabled, "":
		return PrintModeDisabled, nil
	default:
		return PrintModeDisabled, fmt.Errorf("unsupported print mode %q", config.Printing.Mode)
	}
}

func updatePrintingConfig(mode PrintMode) error {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(cfgDirectory, "config.yml")

	var config Config
	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &config); err != nil {
			return err
		}
	}

	config.Printing.Mode = mode
	b, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, b, 0600)
}

func (p *Printer) ExecutePrint(ctx context.Context, printer string, filename string, data []byte) error {
	tmp, err := os.CreateTemp("", "deskconn-print-*-"+safePrintTempName(filename))
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}

	_, err = p.cups.PrintRaw(ctx, printer, tmp)
	return err
}

type printerCUPSClient struct {
	host string
}

func newPrinterCUPSClient(host string) *printerCUPSClient {
	return &printerCUPSClient{host: host}
}

func (c *printerCUPSClient) cupsClient() *ipp.CUPSClient {
	return ipp.NewCUPSClientWithAdapter("", ipp.NewSocketAdapter(c.host, false))
}

func (c *printerCUPSClient) ippClient() *ipp.IPPClient {
	return ipp.NewIPPClientWithAdapter("", ipp.NewSocketAdapter(c.host, false))
}

func (c *printerCUPSClient) PrintersInfo(ctx context.Context) ([]PrinterInfo, error) {
	printers, err := c.cupsClient().GetPrintersContext(ctx, []string{"printer-name", "ppd-name"})
	if err != nil {
		return nil, err
	}

	result := make([]PrinterInfo, 0, len(printers))
	for name, attrs := range printers {
		result = append(result, PrinterInfo{Name: name, PPDModel: attrString(attrs, "ppd-name")})
	}
	return result, nil
}

func (c *printerCUPSClient) PrintRaw(ctx context.Context, printer string, f *os.File) (int, error) {
	mimeType, err := detectPrintMimeType(f)
	if err != nil {
		return -1, err
	}

	stat, err := f.Stat()
	if err != nil {
		return -1, err
	}

	return c.ippClient().PrintDocumentsContext(ctx, []ipp.Document{{
		Document: f,
		Name:     filepath.Base(f.Name()),
		Size:     int(stat.Size()),
		MimeType: mimeType,
	}}, printer, nil)
}

func detectPrintMimeType(r io.ReadSeeker) (string, error) {
	header := make([]byte, 8)
	n, err := r.Read(header)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	sample := string(header[:n])
	switch {
	case strings.HasPrefix(sample, "%PDF-"):
		return "application/pdf", nil
	case strings.HasPrefix(sample, "%!PS-Adobe-"), strings.HasPrefix(sample, "%!PS"):
		return "application/postscript", nil
	default:
		return "application/octet-stream", nil
	}
}

func attrString(attrs ipp.Attributes, key string) string {
	if vals, ok := attrs[key]; ok && len(vals) > 0 {
		if s, ok := vals[0].Value.(string); ok {
			return s
		}
	}
	return ""
}

func safePrintTempName(filename string) string {
	name := filepath.Base(strings.TrimSpace(filename))
	name = strings.ReplaceAll(name, string(filepath.Separator), "_")
	if name == "" || name == "." {
		return "print-job"
	}
	return name
}
