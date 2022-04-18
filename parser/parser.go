package parser

import "flashcat.cloud/categraf/types"

type Parser interface {
	Parse(input []byte) ([]types.Metric, error)
}
