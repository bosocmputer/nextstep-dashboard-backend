package sml

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"strings"
)

const soapNamespace = "http://SMLWebService/"

var (
	ErrZIPFormatInvalid = errors.New("JavaWS ZIP format is invalid")
	ErrZIPEmpty         = errors.New("JavaWS ZIP archive is empty")
	ErrZIPTooLarge      = errors.New("JavaWS ZIP payload exceeds the response limit")
	ErrZIPReadFailed    = errors.New("JavaWS ZIP entry could not be read")
)

type ResultValidationCode string

const (
	ResultValidationParserConfigurationInvalid ResultValidationCode = "PARSER_CONFIGURATION_INVALID"
	ResultValidationXMLMalformed               ResultValidationCode = "XML_MALFORMED"
	ResultValidationResultSetMissing           ResultValidationCode = "RESULT_SET_MISSING"
	ResultValidationRowLimitExceeded           ResultValidationCode = "ROW_LIMIT_EXCEEDED"
	ResultValidationRowMalformed               ResultValidationCode = "ROW_MALFORMED"
	ResultValidationFieldMalformed             ResultValidationCode = "FIELD_MALFORMED"
	ResultValidationFieldValueTooLarge         ResultValidationCode = "FIELD_VALUE_TOO_LARGE"
)

// ResultValidationError contains only bounded parser metadata. It intentionally
// excludes the response body, field names, and field values.
type ResultValidationError struct {
	Code          ResultValidationCode
	OffsetBytes   int64
	RowsDecoded   int
	ResultSetSeen bool
	cause         error
}

func (err *ResultValidationError) Error() string { return err.cause.Error() }
func (err *ResultValidationError) Unwrap() error { return err.cause }

func resultValidationError(code ResultValidationCode, decoder *xml.Decoder, rowsDecoded int, resultSetSeen bool, cause error) *ResultValidationError {
	offset := int64(0)
	if decoder != nil {
		offset = decoder.InputOffset()
	}
	return &ResultValidationError{Code: code, OffsetBytes: offset, RowsDecoded: rowsDecoded, ResultSetSeen: resultSetSeen, cause: cause}
}

func CompressPayload(payload []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("0")
	if err != nil {
		return nil, errors.New("create JavaWS ZIP entry")
	}
	if _, err := entry.Write(payload); err != nil {
		return nil, errors.New("write JavaWS ZIP entry")
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("close JavaWS ZIP payload")
	}
	return buffer.Bytes(), nil
}

func DecompressPayload(payload []byte, maximumBytes int64) ([]byte, error) {
	if maximumBytes < 1 {
		return nil, errors.New("JavaWS decompression limit is invalid")
	}
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, ErrZIPFormatInvalid
	}
	if len(reader.File) == 0 {
		return nil, ErrZIPEmpty
	}
	file := reader.File[0]
	if file.UncompressedSize64 > uint64(maximumBytes) {
		return nil, ErrZIPTooLarge
	}
	entry, err := file.Open()
	if err != nil {
		return nil, ErrZIPReadFailed
	}
	defer entry.Close()
	contents, err := io.ReadAll(io.LimitReader(entry, maximumBytes+1))
	if err != nil {
		return nil, ErrZIPReadFailed
	}
	if int64(len(contents)) > maximumBytes {
		return nil, ErrZIPTooLarge
	}
	return contents, nil
}

func BuildQueryEnvelope(guid, configFileName, databaseName, compressedQueryBase64 string) string {
	escape := func(value string) string { return html.EscapeString(value) }
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">` +
		`<soap:Body><_queryCompress xmlns="` + soapNamespace + `">` +
		`<arg0 xmlns="">` + escape(guid) + `</arg0>` +
		`<arg1 xmlns="">` + escape(configFileName) + `</arg1>` +
		`<arg2 xmlns="">` + escape(databaseName) + `</arg2>` +
		`<arg3 xmlns="">` + compressedQueryBase64 + `</arg3>` +
		`</_queryCompress></soap:Body></soap:Envelope>`
}

func ExtractSOAPReturn(payload []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", errors.New("JavaWS SOAP response did not include a return payload")
		}
		if err != nil {
			return "", errors.New("JavaWS SOAP response could not be parsed")
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "fault":
			return "", errors.New("JavaWS returned a SOAP fault")
		case "return":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return "", errors.New("JavaWS SOAP return payload could not be parsed")
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return "", errors.New("JavaWS SOAP response returned an empty payload")
			}
			return value, nil
		}
	}
}

func ParseRows(payload []byte, maximumRows int) ([]map[string]string, error) {
	if maximumRows < 1 {
		return nil, resultValidationError(ResultValidationParserConfigurationInvalid, nil, 0, false, errors.New("JavaWS row limit is invalid"))
	}
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	rows := make([]map[string]string, 0)
	seenResultSet := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, resultValidationError(ResultValidationXMLMalformed, decoder, len(rows), seenResultSet, errors.New("JavaWS XML response could not be parsed"))
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "resultset":
			seenResultSet = true
		case "row":
			if !seenResultSet {
				continue
			}
			if len(rows) >= maximumRows {
				return nil, resultValidationError(ResultValidationRowLimitExceeded, decoder, len(rows), seenResultSet, fmt.Errorf("JavaWS row count exceeds limit %d", maximumRows))
			}
			row, err := decodeRow(decoder, start)
			if err != nil {
				var validationError *ResultValidationError
				if errors.As(err, &validationError) {
					validationError.RowsDecoded = len(rows)
					validationError.ResultSetSeen = seenResultSet
				}
				return nil, err
			}
			rows = append(rows, row)
		}
	}
	if !seenResultSet {
		return nil, resultValidationError(ResultValidationResultSetMissing, decoder, len(rows), false, errors.New("JavaWS XML response did not include ResultSet"))
	}
	return rows, nil
}

func decodeRow(decoder *xml.Decoder, rowStart xml.StartElement) (map[string]string, error) {
	row := make(map[string]string)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, resultValidationError(ResultValidationRowMalformed, decoder, 0, false, errors.New("JavaWS row could not be parsed"))
		}
		switch typed := token.(type) {
		case xml.StartElement:
			var value string
			if err := decoder.DecodeElement(&value, &typed); err != nil {
				return nil, resultValidationError(ResultValidationFieldMalformed, decoder, 0, false, errors.New("JavaWS row field could not be parsed"))
			}
			if len(value) > 1024*1024 {
				return nil, resultValidationError(ResultValidationFieldValueTooLarge, decoder, 0, false, errors.New("JavaWS row field exceeds the value limit"))
			}
			row[typed.Name.Local] = value
		case xml.EndElement:
			if typed.Name.Local == rowStart.Name.Local {
				return row, nil
			}
		}
	}
}
