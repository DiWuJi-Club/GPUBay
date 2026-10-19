// Package csvx 输出后台导出用的 CSV：UTF-8 带 BOM（Excel 直接打开不乱码），金额按元两位小数，时间按北京时间，并防止公式注入。
package csvx

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	"spark-cloud/internal/platform/apperr"
)

// MaxRows 是单次导出的最大行数。
const MaxRows = 100_000

var ErrTooLarge = apperr.New("export_too_large", "导出行数超过 10 万，请缩小时间范围", 400)

var beijing = time.FixedZone("CST", 8*3600)

type Writer struct {
	w *csv.Writer
}

func NewWriter(w io.Writer) (*Writer, error) {
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return nil, err
	}
	return &Writer{w: csv.NewWriter(w)}, nil
}

func (w *Writer) Write(record []string) error {
	out := make([]string, len(record))
	for i, v := range record {
		out[i] = escapeFormula(v)
	}
	return w.w.Write(out)
}

func (w *Writer) Flush() error {
	w.w.Flush()
	return w.w.Error()
}

// escapeFormula 给可能被表格软件当成公式的单元格加单引号前缀；负数金额保持原样。
func escapeFormula(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + v
	case '-':
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return v
		}
		return "'" + v
	}
	return v
}

// Yuan 把分格式化为元，保留两位小数。
func Yuan(fen int64) string {
	sign := ""
	if fen < 0 {
		sign, fen = "-", -fen
	}
	return fmt.Sprintf("%s%d.%02d", sign, fen/100, fen%100)
}

// Time 以北京时间格式化。
func Time(t time.Time) string { return t.In(beijing).Format("2006-01-02 15:04:05") }

func TimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return Time(*t)
}
