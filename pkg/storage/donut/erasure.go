package donut

import (
	"bytes"
	"io"
	"strconv"
	"time"

	"crypto/md5"
	"encoding/hex"
	"errors"
	"github.com/minio-io/minio/pkg/encoding/erasure"
	"github.com/minio-io/minio/pkg/utils/split"
	"strings"
)

func erasureReader(readers []io.ReadCloser, donutMetadata map[string]string, writer *io.PipeWriter) {
	// TODO handle errors
	totalChunks, _ := strconv.Atoi(donutMetadata["chunkCount"])
	totalLeft, _ := strconv.Atoi(donutMetadata["totalLength"])
	blockSize, _ := strconv.Atoi(donutMetadata["blockSize"])
	k, _ := strconv.Atoi(donutMetadata["erasureK"])
	m, _ := strconv.Atoi(donutMetadata["erasureM"])
	expectedMd5sum, _ := hex.DecodeString(donutMetadata["md5"])
	summer := md5.New()
	// TODO select technique properly
	params, _ := erasure.ParseEncoderParams(uint8(k), uint8(m), erasure.Cauchy)
	encoder := erasure.NewEncoder(params)
	for _, reader := range readers {
		defer reader.Close()
	}
	for i := 0; i < totalChunks; i++ {
		curBlockSize := totalLeft
		if blockSize < totalLeft {
			curBlockSize = blockSize
		}
		curChunkSize := erasure.GetEncodedChunkLen(curBlockSize, uint8(k))

		encodedBytes := make([][]byte, 16)
		for i, reader := range readers {
			var bytesBuffer bytes.Buffer
			// TODO watch for errors
			io.CopyN(&bytesBuffer, reader, int64(curChunkSize))
			encodedBytes[i] = bytesBuffer.Bytes()
		}
		decodedData, err := encoder.Decode(encodedBytes, curBlockSize)
		if err != nil {
			writer.CloseWithError(err)
			return
		}
		summer.Write(decodedData)
		io.Copy(writer, bytes.NewBuffer(decodedData))
		totalLeft = totalLeft - blockSize
	}
	actualMd5sum := summer.Sum(nil)
	if bytes.Compare(expectedMd5sum, actualMd5sum) != 0 {
		writer.CloseWithError(errors.New("decoded md5sum did not match"))
		return
	}
	writer.Close()
}

// erasure writer

type erasureWriter struct {
	writers       []Writer
	metadata      map[string]string
	donutMetadata map[string]string // not exposed
	erasureWriter *io.PipeWriter
	isClosed      <-chan bool
}

func newErasureWriter(writers []Writer) ObjectWriter {
	r, w := io.Pipe()
	isClosed := make(chan bool)
	writer := erasureWriter{
		writers:       writers,
		metadata:      make(map[string]string),
		erasureWriter: w,
		isClosed:      isClosed,
	}
	go erasureGoroutine(r, writer, isClosed)
	return writer
}

func erasureGoroutine(r *io.PipeReader, eWriter erasureWriter, isClosed chan<- bool) {
	chunks := split.Stream(r, 10*1024*1024)
	params, _ := erasure.ParseEncoderParams(8, 8, erasure.Cauchy)
	encoder := erasure.NewEncoder(params)
	chunkCount := 0
	totalLength := 0
	summer := md5.New()
	for chunk := range chunks {
		if chunk.Err == nil {
			totalLength = totalLength + len(chunk.Data)
			encodedBlocks, _ := encoder.Encode(chunk.Data)
			summer.Write(chunk.Data)
			for blockIndex, block := range encodedBlocks {
				io.Copy(eWriter.writers[blockIndex], bytes.NewBuffer(block))
			}
		}
		chunkCount = chunkCount + 1
	}
	dataMd5sum := summer.Sum(nil)
	metadata := make(map[string]string)
	metadata["blockSize"] = strconv.Itoa(10 * 1024 * 1024)
	metadata["chunkCount"] = strconv.Itoa(chunkCount)
	metadata["created"] = time.Now().Format(time.RFC3339Nano)
	metadata["erasureK"] = "8"
	metadata["erasureM"] = "8"
	metadata["erasureTechnique"] = "Cauchy"
	metadata["md5"] = hex.EncodeToString(dataMd5sum)
	metadata["totalLength"] = strconv.Itoa(totalLength)
	for _, nodeWriter := range eWriter.writers {
		if nodeWriter != nil {
			nodeWriter.SetMetadata(eWriter.metadata)
			nodeWriter.SetDonutMetadata(metadata)
			nodeWriter.Close()
		}
	}
	isClosed <- true
}

func (eWriter erasureWriter) Write(data []byte) (int, error) {
	io.Copy(eWriter.erasureWriter, bytes.NewBuffer(data))
	return len(data), nil
}

func (eWriter erasureWriter) Close() error {
	eWriter.erasureWriter.Close()
	<-eWriter.isClosed
	return nil
}

func (eWriter erasureWriter) CloseWithError(err error) error {
	for _, writer := range eWriter.writers {
		if writer != nil {
			writer.CloseWithError(err)
		}
	}
	return nil
}

func (eWriter erasureWriter) SetMetadata(metadata map[string]string) error {
	for k, _ := range metadata {
		if strings.HasPrefix(k, "sys.") {
			return errors.New("Invalid key '" + k + "', cannot start with sys.'")
		}
	}
	for k := range eWriter.metadata {
		delete(eWriter.metadata, k)
	}
	for k, v := range metadata {
		eWriter.metadata[k] = v
	}
	return nil
}

func (eWriter erasureWriter) GetMetadata() (map[string]string, error) {
	metadata := make(map[string]string)
	for k, v := range eWriter.metadata {
		metadata[k] = v
	}
	return metadata, nil
}
