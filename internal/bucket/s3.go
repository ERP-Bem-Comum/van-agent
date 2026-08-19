// Implementação de `Store` sobre S3 — a metade desta fronteira que finalmente atravessa.
//
// A dependência do SDK oficial entra aqui, e só aqui: o resto do agente continua sem `require`
// algum, e o ciclo continua exercitável contra `Memory` sem nuvem e sem dinheiro real. O SDK é o
// oficial, sem wrapper caseiro, pela mesma razão que o core-api adotou no adapter dele — quem
// escreve um cliente S3 à mão passa a manter assinatura v4, retentativa e paginação, três coisas
// que o fornecedor mantém melhor.
//
// ⚠️ Nenhum valor deste arquivo aponta para bucket real. Bucket, região e endpoint entram por
// ambiente na instância (ADR-0061 §5), e este repositório é público.
package bucket

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// StaticCredentials é o par de chaves quando ele é informado explicitamente.
//
// O caminho de produção NÃO passa por aqui: a credencial é a role da instância (ADR-0061 §5),
// resolvida pela cadeia de provedores. Este par existe para o endpoint local do teste de
// integração, onde não há metadados de instância para consultar.
type StaticCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// String redige o segredo.
//
// Não é zelo decorativo: o modo de vazamento real é alguém depurar um erro de boot com `%v` sobre a
// configuração inteira e o valor ir para o log de uma máquina que ninguém rotaciona. Redigir no
// tipo faz a proteção viajar junto com o dado, em vez de depender de quem formata.
func (c StaticCredentials) String() string {
	return fmt.Sprintf("{AccessKeyID:%s SecretAccessKey:[REDIGIDO]}", c.AccessKeyID)
}

// S3Config descreve o bucket. Espelha, campo a campo, o que o core-api resolve em
// `van-s3-config.ts`: os dois lados leem o MESMO conjunto de variáveis, com as mesmas regras, para
// que não exista o desfecho em que um escreve num lugar e o outro procura noutro.
type S3Config struct {
	Bucket string
	Region string
	// Endpoint vazio significa AWS. Preenchido, aponta para um S3-compatível (MinIO no teste de
	// integração, ou outro provedor).
	Endpoint string
	// ForcePathStyle liga o endereçamento por caminho (`endpoint/bucket/chave`) em vez de por
	// subdomínio. É o que o endpoint local exige: `bucket.localhost` não resolve.
	ForcePathStyle bool
	// Credentials nil ⇒ cadeia de provedores (role da instância/IMDS). É o caminho de produção.
	Credentials *StaticCredentials
}

// String redige a credencial ao imprimir a configuração inteira.
func (c S3Config) String() string {
	cred := "cadeia de provedores (role da instância)"
	if c.Credentials != nil {
		cred = c.Credentials.String()
	}
	return fmt.Sprintf("{Bucket:%s Region:%s Endpoint:%s ForcePathStyle:%t Credentials:%s}",
		c.Bucket, c.Region, c.Endpoint, c.ForcePathStyle, cred)
}

// Validate recusa configuração incompleta antes de qualquer chamada de rede.
//
// É rede de segurança, não a validação principal: quem nomeia a VARIÁVEL faltante é o pacote
// `config`, porque é ele que conhece os nomes. Aqui a checagem se repete para quem montar a struct
// em código.
func (c S3Config) Validate() error {
	switch {
	case c.Bucket == "":
		return errors.New("nome do bucket não configurado")
	case c.Region == "":
		return errors.New("região do bucket não configurada")
	}
	if c.Credentials != nil {
		if c.Credentials.AccessKeyID == "" || c.Credentials.SecretAccessKey == "" {
			// Credencial pela metade nunca é intenção: é uma variável que ficou para trás. Tentar
			// autenticar assim produz um erro de assinatura no meio do ciclo, longe da causa.
			return errors.New("credencial estática incompleta: as duas chaves precisam ser informadas")
		}
	}
	return nil
}

// S3 é a implementação de `Store` sobre o object storage real.
//
// A asserção abaixo é o que garante que o adapter é implementação ADICIONAL da interface, e não
// substituição do duplo: os dois precisam continuar satisfazendo o mesmo contrato, porque é o duplo
// que sustenta a suíte inteira sem nuvem e sem dinheiro real.
var _ Store = (*S3)(nil)

type S3 struct {
	client *s3.Client
	bucket string
}

// NewS3 monta o cliente.
//
// A resolução da credencial acontece AQUI, no boot, e não na primeira operação: uma instância sem
// role atribuída precisa falhar antes de o ciclo começar, não depois de a listagem já ter decidido
// o que transmitir.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.Credentials != nil {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.Credentials.AccessKeyID, cfg.Credentials.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		// A mensagem do SDK não carrega credencial, mas o embrulho é nosso e precisa continuar não
		// carregando: nada de `%v` sobre a configuração aqui.
		return nil, fmt.Errorf("resolver credencial do object storage: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})

	return &S3{client: client, bucket: cfg.Bucket}, nil
}

// List devolve as chaves sob um prefixo, em ordem estável.
//
// Pagina de propósito: `ListObjectsV2` devolve no máximo 1000 chaves por resposta, e uma fila que
// passou de mil é exatamente o dia em que ninguém está olhando. Truncar silenciosamente deixaria
// remessas paradas sem erro em lugar nenhum.
//
// A ordenação repete a garantia do duplo: o ciclo processa na mesma ordem em que a lista chega, e
// ordem instável tornaria irreprodutível qualquer investigação sobre o que foi tratado primeiro.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	out := []string{}
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listar prefixo %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			// Alguns provedores materializam a "pasta" como um objeto de tamanho zero com a chave
			// igual ao prefixo. Tratá-lo como remessa faria o ciclo tentar transmitir um nome vazio.
			if *obj.Key == prefix {
				continue
			}
			out = append(out, *obj.Key)
		}
	}

	sort.Strings(out)
	return out, nil
}

// Get lê um objeto.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	res, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			// A distinção não é cosmética: ausente é concorrência normal (outra passada já moveu o
			// objeto) e o ciclo segue; falha de leitura é infraestrutura e precisa aparecer.
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("ler objeto %q: %w", key, err)
	}
	defer res.Body.Close()

	content, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("ler corpo do objeto %q: %w", key, err)
	}
	return content, nil
}

// Put escreve um objeto.
func (s *S3) Put(ctx context.Context, key string, content []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(content),
	})
	if err != nil {
		return fmt.Errorf("escrever objeto %q: %w", key, err)
	}
	return nil
}

// Move reposiciona o objeto: copia para o destino, CONFERE que o destino existe, e só então remove
// a origem.
//
// ⚠️ Esta é a única remoção do agente, e ela não contradiz "o agente nunca apaga" (ADR-0061 §1) —
// ela é o que torna a regra verdadeira. O S3 não tem operação de mover: a alternativa a copiar e
// remover seria deixar o objeto na origem, e aí o próximo ciclo o encontraria de novo na fila. O
// que a regra proíbe é apagar como OPERAÇÃO — por isso `Store` não tem `Delete`, e por isso o código
// que apagaria não compila em lugar nenhum do ciclo.
//
// Duas garantias sustentam isso, e as duas precisam existir na instalação:
//
//   - a ordem é copiar → conferir → remover. Remover antes de confirmar a cópia perderia a remessa,
//     que é o desfecho pior que qualquer duplicidade de status;
//   - o bucket é VERSIONADO, e a remoção da origem vira marcador de deleção com a versão anterior
//     preservada. Sem versionamento a movimentação continua correta, mas o histórico deixa de
//     existir — e o histórico é o que permite reconstruir o que foi transmitido.
func (s *S3) Move(ctx context.Context, from, to string) error {
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(to),
		CopySource: aws.String(copySource(s.bucket, from)),
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, from)
		}
		return fmt.Errorf("copiar %q para %q: %w", from, to, err)
	}

	// A conferência custa uma chamada e compra a garantia de que a remoção seguinte não é sobre uma
	// cópia que não existe. Um `CopyObject` pode devolver 200 com erro no corpo — comportamento
	// documentado do S3 — e é justamente esse o caso que o erro de retorno sozinho não pega.
	if _, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(to),
	}); err != nil {
		return fmt.Errorf("conferir a cópia em %q antes de remover a origem %q: %w", to, from, err)
	}

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(from),
	}); err != nil {
		// A cópia já existe: o objeto está nos dois lugares. Isso é visível e recuperável — ao
		// contrário do inverso, que seria não estar em lugar nenhum.
		return fmt.Errorf("remover a origem %q depois de copiar para %q: %w", from, to, err)
	}
	return nil
}

// copySource monta o `CopySource` do `CopyObject`, que é `bucket/chave` com escape de URL.
//
// O escape é por SEGMENTO, preservando as barras: escapar a string inteira transformaria as barras
// do prefixo em `%2F` e o S3 procuraria uma chave literal com esses caracteres — que não existe.
func copySource(bucketName, key string) string {
	u := url.URL{Path: bucketName + "/" + key}
	return u.EscapedPath()
}

// isNotFound reconhece "o objeto não existe" nas várias formas em que o S3 e os compatíveis o
// dizem.
//
// A tolerância é deliberada: o `HeadObject` responde `NotFound` sem corpo, o `GetObject` responde
// `NoSuchKey`, e provedores compatíveis nem sempre tipam o erro. Confundir ausência com
// indisponibilidade faria o ciclo alarmar sobre concorrência normal; o inverso é que seria grave, e
// não é o que acontece aqui — um erro real de infraestrutura não traz nenhum destes códigos.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
