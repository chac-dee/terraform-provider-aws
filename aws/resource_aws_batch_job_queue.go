package aws

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/batch"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/terraform-providers/terraform-provider-aws/aws/internal/keyvaluetags"
)

func resourceAwsBatchJobQueue() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsBatchJobQueueCreate,
		Read:   resourceAwsBatchJobQueueRead,
		Update: resourceAwsBatchJobQueueUpdate,
		Delete: resourceAwsBatchJobQueueDelete,

		Importer: &schema.ResourceImporter{
			State: func(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
				d.Set("arn", d.Id())
				return []*schema.ResourceData{d}, nil
			},
		},

		Schema: map[string]*schema.Schema{
			"compute_environment_order": {
				Type:     schema.TypeList,
				Required: true,
				MaxItems: 3,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"compute_environment": {
							Type:     schema.TypeString,
							Required: true,
						},
						"order": {
							Type:         schema.TypeInt,
							Required:     true,
							ValidateFunc: validation.IntAtLeast(0),
						},
					},
				},
			},
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validateBatchName,
			},
			"priority": {
				Type:     schema.TypeInt,
				Required: true,
			},
			"state": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringInSlice([]string{batch.JQStateDisabled, batch.JQStateEnabled}, true),
			},
			"tags":     tagsSchema(),
			"tags_all": tagsSchemaComputed(),
			"arn": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},

		CustomizeDiff: SetTagsDiff,
	}
}

func resourceAwsBatchJobQueueCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).batchconn
	defaultTagsConfig := meta.(*AWSClient).DefaultTagsConfig
	tags := defaultTagsConfig.MergeTags(keyvaluetags.New(d.Get("tags").(map[string]interface{})))

	input := batch.CreateJobQueueInput{
		ComputeEnvironmentOrder: createComputeEnvironmentOrder(d.Get("compute_environment_order").([]interface{})),
		JobQueueName:            aws.String(d.Get("name").(string)),
		Priority:                aws.Int64(int64(d.Get("priority").(int))),
		State:                   aws.String(d.Get("state").(string)),
	}
	if len(tags) > 0 {
		input.Tags = tags.IgnoreAws().BatchTags()
	}

	name := d.Get("name").(string)
	out, err := conn.CreateJobQueue(&input)
	if err != nil {
		return fmt.Errorf("%s %q", err, name)
	}

	stateConf := &resource.StateChangeConf{
		Pending:    []string{batch.JQStatusCreating, batch.JQStatusUpdating},
		Target:     []string{batch.JQStatusValid},
		Refresh:    batchJobQueueRefreshStatusFunc(conn, name),
		Timeout:    10 * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}

	_, err = stateConf.WaitForState()
	if err != nil {
		return fmt.Errorf("Error waiting for JobQueue state to be \"VALID\": %s", err)
	}

	arn := aws.StringValue(out.JobQueueArn)
	log.Printf("[DEBUG] JobQueue created: %s", arn)
	d.SetId(arn)

	return resourceAwsBatchJobQueueRead(d, meta)
}

func resourceAwsBatchJobQueueRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).batchconn
	defaultTagsConfig := meta.(*AWSClient).DefaultTagsConfig
	ignoreTagsConfig := meta.(*AWSClient).IgnoreTagsConfig

	jq, err := getJobQueue(conn, d.Id())
	if err != nil {
		return err
	}
	if jq == nil {
		log.Printf("[WARN] Batch Job Queue (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}

	d.Set("arn", jq.JobQueueArn)

	computeEnvironments := make([]string, 0, len(jq.ComputeEnvironmentOrder))

	sort.Slice(jq.ComputeEnvironmentOrder, func(i, j int) bool {
		return aws.Int64Value(jq.ComputeEnvironmentOrder[i].Order) < aws.Int64Value(jq.ComputeEnvironmentOrder[j].Order)
	})

	for _, computeEnvironmentOrder := range jq.ComputeEnvironmentOrder {
		computeEnvironments = append(computeEnvironments, aws.StringValue(computeEnvironmentOrder.ComputeEnvironment))
	}

	if err := d.Set("compute_environments", computeEnvironments); err != nil {
		return fmt.Errorf("error setting compute_environments: %s", err)
	}

	d.Set("name", jq.JobQueueName)
	d.Set("priority", jq.Priority)
	d.Set("state", jq.State)

	tags := keyvaluetags.BatchKeyValueTags(jq.Tags).IgnoreAws().IgnoreConfig(ignoreTagsConfig)

	//lintignore:AWSR002
	if err := d.Set("tags", tags.RemoveDefaultConfig(defaultTagsConfig).Map()); err != nil {
		return fmt.Errorf("error setting tags: %w", err)
	}

	if err := d.Set("tags_all", tags.Map()); err != nil {
		return fmt.Errorf("error setting tags_all: %w", err)
	}

	return nil
}

func resourceAwsBatchJobQueueUpdate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).batchconn

	if d.HasChanges("compute_environments", "priority", "state") {
		name := d.Get("name").(string)

		updateInput := &batch.UpdateJobQueueInput{
			ComputeEnvironmentOrder: createComputeEnvironmentOrder(d.Get("compute_environment_order").([]interface{})),
			JobQueue:                aws.String(name),
			Priority:                aws.Int64(int64(d.Get("priority").(int))),
			State:                   aws.String(d.Get("state").(string)),
		}
		_, err := conn.UpdateJobQueue(updateInput)
		if err != nil {
			return err
		}
		stateConf := &resource.StateChangeConf{
			Pending:    []string{batch.JQStatusUpdating},
			Target:     []string{batch.JQStatusValid},
			Refresh:    batchJobQueueRefreshStatusFunc(conn, name),
			Timeout:    10 * time.Minute,
			Delay:      10 * time.Second,
			MinTimeout: 3 * time.Second,
		}

		_, err = stateConf.WaitForState()
		if err != nil {
			return err
		}
	}

	if d.HasChange("tags_all") {
		o, n := d.GetChange("tags_all")

		if err := keyvaluetags.BatchUpdateTags(conn, d.Get("arn").(string), o, n); err != nil {
			return fmt.Errorf("error updating tags: %s", err)
		}
	}

	return resourceAwsBatchJobQueueRead(d, meta)
}

func resourceAwsBatchJobQueueDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).batchconn
	name := d.Get("name").(string)

	log.Printf("[DEBUG] Disabling Batch Job Queue %s", name)
	err := disableBatchJobQueue(name, conn)
	if err != nil {
		return fmt.Errorf("error disabling Batch Job Queue (%s): %s", name, err)
	}

	log.Printf("[DEBUG] Deleting Batch Job Queue %s", name)
	err = deleteBatchJobQueue(name, conn)
	if err != nil {
		return fmt.Errorf("error deleting Batch Job Queue (%s): %s", name, err)
	}

	return nil
}

func createComputeEnvironmentOrder(order []interface{}) (envs []*batch.ComputeEnvironmentOrder) {
	for _, env := range order {
		m := env.(map[string]interface{})
		envs = append(envs, &batch.ComputeEnvironmentOrder{
			Order:              aws.Int64(m["order"].(int64)),
			ComputeEnvironment: aws.String(m["compute_environment"].(string)),
		})
	}
	return
}

func deleteBatchJobQueue(jobQueue string, conn *batch.Batch) error {
	_, err := conn.DeleteJobQueue(&batch.DeleteJobQueueInput{
		JobQueue: aws.String(jobQueue),
	})
	if err != nil {
		return err
	}

	stateChangeConf := &resource.StateChangeConf{
		Pending:    []string{batch.JQStateDisabled, batch.JQStatusDeleting},
		Target:     []string{batch.JQStatusDeleted},
		Refresh:    batchJobQueueRefreshStatusFunc(conn, jobQueue),
		Timeout:    10 * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}

	_, err = stateChangeConf.WaitForState()
	return err
}

func disableBatchJobQueue(jobQueue string, conn *batch.Batch) error {
	_, err := conn.UpdateJobQueue(&batch.UpdateJobQueueInput{
		JobQueue: aws.String(jobQueue),
		State:    aws.String(batch.JQStateDisabled),
	})
	if err != nil {
		return err
	}

	stateChangeConf := &resource.StateChangeConf{
		Pending:    []string{batch.JQStatusUpdating},
		Target:     []string{batch.JQStatusValid},
		Refresh:    batchJobQueueRefreshStatusFunc(conn, jobQueue),
		Timeout:    10 * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}
	_, err = stateChangeConf.WaitForState()
	return err
}

func getJobQueue(conn *batch.Batch, sn string) (*batch.JobQueueDetail, error) {
	describeOpts := &batch.DescribeJobQueuesInput{
		JobQueues: []*string{aws.String(sn)},
	}
	resp, err := conn.DescribeJobQueues(describeOpts)
	if err != nil {
		return nil, err
	}

	numJobQueues := len(resp.JobQueues)
	switch {
	case numJobQueues == 0:
		log.Printf("[DEBUG] Job Queue %q is already gone", sn)
		return nil, nil
	case numJobQueues == 1:
		return resp.JobQueues[0], nil
	case numJobQueues > 1:
		return nil, fmt.Errorf("Multiple Job Queues with name %s", sn)
	}
	return nil, nil
}

func batchJobQueueRefreshStatusFunc(conn *batch.Batch, sn string) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		ce, err := getJobQueue(conn, sn)
		if err != nil {
			return nil, "failed", err
		}
		if ce == nil {
			return 42, batch.JQStatusDeleted, nil
		}
		return ce, *ce.Status, nil
	}
}
