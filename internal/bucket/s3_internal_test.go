package bucket

import "testing"

// O `CopySource` do S3 é `bucket/chave` com escape de URL, e o escape precisa ser por SEGMENTO.
//
// O erro que este teste fecha é silencioso e caro: escapar a string inteira transforma as barras do
// prefixo em `%2F`, o S3 procura uma chave literal com esses caracteres, não acha, e a movimentação
// falha com "objeto não encontrado" sobre um objeto que está lá.
func TestCopySourceEscapaSemDestruirOsPrefixos(t *testing.T) {
	casos := []struct {
		nome     string
		bucket   string
		key      string
		esperado string
	}{
		{"chave com prefixo", "meu-bucket", "saida/PAG_000000_000001.REM", "meu-bucket/saida/PAG_000000_000001.REM"},
		{"chave com espaço", "meu-bucket", "saida/COM ESPACO.REM", "meu-bucket/saida/COM%20ESPACO.REM"},
		{"chave na raiz", "meu-bucket", "X.REM", "meu-bucket/X.REM"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := copySource(c.bucket, c.key); got != c.esperado {
				t.Errorf("copySource = %q, esperava %q", got, c.esperado)
			}
		})
	}
}
