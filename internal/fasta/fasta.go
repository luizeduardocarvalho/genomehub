package fasta

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type Chromosome struct {
	Name     string
	Sequence []byte
}

func Read(path string) ([]Chromosome, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var chroms []Chromosome
	var current *Chromosome
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ">") {
			if current != nil {
				chroms = append(chroms, *current)
			}
			name := strings.TrimPrefix(line, ">")
			if i := strings.IndexAny(name, " \t"); i != -1 {
				name = name[:i]
			}
			current = &Chromosome{Name: name}
		} else if current != nil {
			seq := strings.ToUpper(strings.TrimRight(line, "\r\n "))
			current.Sequence = append(current.Sequence, []byte(seq)...)
		}
	}
	if current != nil {
		chroms = append(chroms, *current)
	}
	return chroms, scanner.Err()
}

func Write(path string, chroms []Chromosome) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriterSize(f, 4*1024*1024)
	for _, c := range chroms {
		fmt.Fprintf(w, ">%s\n", c.Name)
		for i := 0; i < len(c.Sequence); i += 60 {
			end := min(i + 60, len(c.Sequence))
			w.Write(c.Sequence[i:end])
			w.WriteByte('\n')
		}
	}
	return w.Flush()
}
