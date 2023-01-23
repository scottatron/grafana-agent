import React, { FC, Fragment, ReactElement } from 'react';
import { gray } from 'd3';

import { ObjectField, Value, ValueType } from './types';

import styles from './RiverValue.module.css';

export interface RiverValueProps {
  value: Value;
  name?: string;
  nthChild?: number;
}

/**
 * RiverValue emits a paragraph which represents a River value.
 */
export const RiverValue: FC<RiverValueProps> = ({ value, name, nthChild }) => {
  return (
    <div className={styles.value}>
      <ValueRenderer value={value} indentLevel={0} nthChild={nthChild} name={name} />
    </div>
  );
};

type valueRendererProps = RiverValueProps & {
  indentLevel: number;
};

type SimplifiedPartition = {
  key: string;
  value: string | false;
};

const ValueRenderer: FC<valueRendererProps> = ({ value, name, nthChild, indentLevel }) => {
  const backgroundColor = nthChild && nthChild % 2 === 1 ? '#f4f5f5' : 'white';
  const renderGrid = (simplifiedPartition: SimplifiedPartition[], gridTemplateColumns?: string) => {
    return simplifiedPartition.map(({ key, value }) => {
      return (
        <div key={key} className={styles['grid-layout']} style={{ backgroundColor: backgroundColor, gridTemplateColumns }}>
          <div className={`${styles['grid-item']} ${styles['grid-key']}`}>{key}</div>
          <div className={styles['grid-item']}>=</div>
          <div className={`${styles['grid-item']} ${styles['grid-value']}`}>"{value}"</div>
        </div>
      );
    });
  };

  const renderDefault = (partition: ObjectField[], indentLevel: number, keyLength: number) => {
    return partition.map((element, index) => {
      return (
        <Fragment key={index.toString()}>
          {getLinePrefix(indentLevel + 1)}
          <span>{partitionKey(element, keyLength)} = </span>
          <ValueRenderer value={element.value} indentLevel={indentLevel + 1} name={name} />
          <span>,</span>
          <br />
        </Fragment>
      );
    });
  };

  switch (value.type) {
    case ValueType.NULL:
      return <span className={styles.literal}>null</span>;

    case ValueType.NUMBER:
      return <span className={styles.literal}>{value.value.toString()}</span>;

    case ValueType.STRING:
      return <span className={styles.string}>"{escapeString(value.value)}"</span>;

    case ValueType.BOOL:
      if (value.value) {
        return <span className={styles.literal}>true</span>;
      }
      return <span className={styles.literal}>false</span>;
    case ValueType.ARRAY:
      return (
        <>
          <span>[</span>
          {value.value.map((element, idx) => {
            return (
              <Fragment key={idx.toString()}>
                <ValueRenderer value={element} indentLevel={indentLevel} name={name} />
                {idx + 1 < value.value.length ? <span>, </span> : null}
              </Fragment>
            );
          })}
          <span>]</span>
        </>
      );

    case ValueType.OBJECT:
      if (value.value.length === 0) {
        // No elements; return `{}` without any line breaks.
        return (
          <>
            <span>&#123;</span>
            <span>&#125;</span>
          </>
        );
      }

      const partitions = partitionFields(value.value);

      return (
        <>
          <span>&#123;</span>
          <br />
          {partitions.map((partition) => {
            // Find the maximum field length across all fields in this
            // partition.
            const keyLength = partitionKeyLength(partition);

            // Loop through the partition and return a simpler object with key and value pair
            const simplifiedPartition = partition.map(({ key, value }) => {
              return {
                key,
                value: typeof value === 'object' && 'value' in value && (value.value as string),
              };
            });

            const gridTemplateColumns = '10% 1% 69%';

            switch (name) {
              case 'targets':
                return renderGrid(simplifiedPartition);
              case 'labels':
                return renderGrid(simplifiedPartition, gridTemplateColumns);
              default:
                return renderDefault(partition, indentLevel, keyLength);
            }
          })}
          <span>&#125;</span>
        </>
      );

    case ValueType.FUNCTION:
      return <span className={styles.special}>{value.value}</span>;

    case ValueType.CAPSULE:
      return <span className={styles.special}>{value.value}</span>;
  }
};

/**
 * partitionFields partitions fields in an object by fields which should have
 * their equal signs aligned.
 *
 * A field which crosses multiple lines (i.e., recursively contains an object
 * with more than one element) will cause a partition break, placing subsequent
 * fields in another partition.
 */
function partitionFields(fields: ObjectField[]): ObjectField[][] {
  const partitions = [];

  let currentPartition: ObjectField[] = [];
  fields.forEach((field) => {
    currentPartition.push(field);

    if (multilinedValue(field.value)) {
      // Fields which cross multiple lines cause a partition break.
      partitions.push(currentPartition);
      currentPartition = [];
    }
  });

  if (currentPartition.length !== 0) {
    partitions.push(currentPartition);
  }

  return partitions;
}

/** multilinedValue returns true if value recrusively crosses multiple lines. */
function multilinedValue(value: Value): boolean {
  switch (value.type) {
    case ValueType.OBJECT:
      // River objects cross more than one line whenever there is at least one
      // element.
      return value.value.length > 0;

    case ValueType.ARRAY:
      // River arrays cross more than one line if any of their elements cross
      // more than one line.
      return value.value.some((v) => multilinedValue(v));
  }

  // Other values never cross line barriers.
  return false;
}

/**
 * partitionKeyLength returns the length of keys within the partition. The
 * length is determined by the longest field name in the partition.
 */
function partitionKeyLength(partition: ObjectField[]): number {
  let keyLength = 0;

  partition.forEach((f) => {
    const fieldLength = partitionKey(f, 0).length;
    if (fieldLength > keyLength) {
      keyLength = fieldLength;
    }
  });

  return keyLength;
}

/**
 * partitionKey returns the text to use to display a key for a field within a
 * partition.
 */
function partitionKey(field: ObjectField, keyLength: number): string {
  let key = field.key;
  if (!validIdentifier(key)) {
    // Keys which aren't valid identifiers should be wrapped in quotes.
    key = `"${key}"`;
  }

  if (key.length < keyLength) {
    return key + ' '.repeat(keyLength - key.length);
  }
  return key;
}

function getLinePrefix(indentLevel: number): ReactElement | null {
  if (indentLevel === 0) {
    return null;
  }
  return <span>{'\t'.repeat(indentLevel)}</span>;
}

/**
 * validIdentifier reports whether the input is a valid River identifier.
 */
function validIdentifier(input: string): boolean {
  return /^[_a-z][_a-z0-9]*$/i.test(input);
}

/**
 * escapeString escapes special characters in a string so they can be printed
 * inside a River string literal.
 */
function escapeString(input: string): string {
  // TODO(rfratto): this should also escape Unicode characters into \u and \U
  // forms.
  return input.replace(/[\b\f\n\r\t\v\0'"\\]/g, (match) => {
    switch (match) {
      case '\b':
        return '\\b';
      case '\f':
        return '\\f';
      case '\n':
        return '\\n';
      case '\r':
        return '\\r';
      case '\t':
        return '\\t';
      case '\v':
        return '\\v';
      case "'":
        return "\\'";
      case '"':
        return '\\"';
      case '\\':
        return '\\\\';
    }
    return '';
  });
}
