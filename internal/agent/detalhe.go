package agent

import (
	"fmt"
	"unicode/utf8"
)

// maxErroNoDetalhe é quanto de UM erro interpolado cabe no campo `detalhe`, em caracteres.
//
// O número sai de uma conta, não de gosto. O pior ponto de interpolação é o desfecho ambíguo em
// `verdict`, que embute DOIS erros na mesma frase: 91 caracteres de texto fixo + o nome do arquivo
// + 2 × este orçamento. Com 160, um nome de até ~99 caracteres ainda cabe nos
// `envelope.MaxDetailLength` sem que o piso do contrato precise cortar nada.
//
// Erro de sistema operacional não tem teto: ele carrega o caminho completo (`spool.go`, `consultar
// %q: %w`) e a mensagem localizada do Windows. Foi medido: um caminho de pasta de 102 caracteres já
// levava aquele detalhe a 513 — e 102 é ordinário numa plataforma cujo limite clássico é 260.
const maxErroNoDetalhe = 160

// marcaDeCorte fecha um trecho que não coube. Igual à do contrato, de propósito: o operador aprende
// UM símbolo, e ele significa a mesma coisa nos dois lugares — "aqui foi cortado".
const marcaDeCorte = " […]"

// resumirErro limita um erro para caber no `detalhe` sem sacrificar o resto da frase.
//
// A escolha é qual metade se perde, e ela não é neutra. O texto FIXO é o que diz ao operador o que
// fazer — "não retransmitir sem conferência", "conferir a instalação" — e é o que menos pode ser
// cortado. A cauda de um erro do SO é caminho e mensagem localizada: a informação de menor densidade
// da frase inteira. Por isso o corte vem daqui, e não de uma tesoura no fim do `detalhe`: cortar por
// trás sacrificaria justamente a instrução.
//
// ⚠️ O corte é por RUNA, nunca por byte. Fatiar bytes partiria um caractere acentuado ao meio e
// produziria JSON inválido para o consumidor — e o texto do Windows em PT-BR é acentuado.
//
// O `nil` continua virando "<nil>", como o `%v` fazia: a frase do desfecho ambíguo nomeia os DOIS
// lados (saída e backup) e apagar o lado que não falhou esconderia QUAL dos dois ficou ilegível.
func resumirErro(err error) string {
	texto := fmt.Sprint(err)
	if utf8.RuneCountInString(texto) <= maxErroNoDetalhe {
		return texto
	}
	runas := []rune(texto)
	return string(runas[:maxErroNoDetalhe-utf8.RuneCountInString(marcaDeCorte)]) + marcaDeCorte
}
