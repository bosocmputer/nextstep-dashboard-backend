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
	if err != nil || len(reader.File) == 0 {
		return nil, errors.New("JavaWS returned an invalid ZIP payload")
	}
	file := reader.File[0]
	if file.UncompressedSize64 > uint64(maximumBytes) {
		return nil, errors.New("JavaWS ZIP payload exceeds the response limit")
	}
	entry, err := file.Open()
	if err != nil {
		return nil, errors.New("open JavaWS ZIP payload")
	}
	defer entry.Close()
	contents, err := io.ReadAll(io.LimitReader(entry, maximumBytes+1))
	if err != nil {
		return nil, errors.New("read JavaWS ZIP payload")
	}
	if int64(len(contents)) > maximumBytes {
		return nil, errors.New("JavaWS ZIP payload exceeds the response limit")
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
		return nil, errors.New("JavaWS row limit is invalid")
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
			return nil, errors.New("JavaWS XML response could not be parsed")
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
				return nil, fmt.Errorf("JavaWS row count exceeds limit %d", maximumRows)
			}
			row, err := decodeRow(decoder, start)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
	}
	if !seenResultSet {
		return nil, errors.New("JavaWS XML response did not include ResultSet")
	}
	return rows, nil
}

func decodeRow(decoder *xml.Decoder, rowStart xml.StartElement) (map[string]string, error) {
	row := make(map[string]string)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("JavaWS row could not be parsed")
		}
		switch typed := token.(type) {
		case xml.StartElement:
			var value string
			if err := decoder.DecodeElement(&value, &typed); err != nil {
				return nil, errors.New("JavaWS row field could not be parsed")
			}
			if len(value) > 1024*1024 {
				return nil, errors.New("JavaWS row field exceeds the value limit")
			}
			row[typed.Name.Local] = value
		case xml.EndElement:
			if typed.Name.Local == rowStart.Name.Local {
				return row, nil
			}
		}
	}
}
